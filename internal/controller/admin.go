package controller

import (
	"crypto/x509"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
	"github.com/NTFespolion307/QuorvexFusion/internal/secret"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// Operations used by the REST API (and therefore the web UI and CLI).

func certPool(ca *pki.CA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// CheckAdminPassword reports whether pw is the admin password.
func (c *Controller) CheckAdminPassword(pw string) bool {
	hash, err := c.store.GetMeta(metaAdminPassword)
	if err != nil || hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// --- join tokens ---

type JoinTokenOptions struct {
	Description string
	ExpiresAt   *time.Time
	MaxUses     *int
	AutoApprove bool
	Location    string
	Ephemeral   bool
}

func createJoinToken(st *store.Store, o JoinTokenOptions) (string, *store.JoinToken, error) {
	token, id, hash := secret.New(secret.PrefixJoin)
	t := &store.JoinToken{
		ID: id, SecretHash: hash, Description: o.Description, CreatedAt: time.Now(),
		ExpiresAt: o.ExpiresAt, MaxUses: o.MaxUses, AutoApprove: o.AutoApprove,
		Location: o.Location, Ephemeral: o.Ephemeral,
	}
	if err := st.CreateJoinToken(t); err != nil {
		return "", nil, err
	}
	return token, t, nil
}

// CreateJoinToken returns the full token string (shown once) and its record.
func (c *Controller) CreateJoinToken(o JoinTokenOptions) (string, *store.JoinToken, error) {
	return createJoinToken(c.store, o)
}

// JoinCommands are ready-to-paste commands for joining a worker.
type JoinCommands struct {
	Install   string `json:"install"`   // from a git clone
	Bootstrap string `json:"bootstrap"` // curl | sh, for cloud-init / vast.ai
	Docker    string `json:"docker"`    // worker container
	Direct    string `json:"direct"`    // binary already installed
}

// RepoURL is where install.sh and release binaries live.
const RepoURL = "https://github.com/NTFespolion307/QuorvexFusion"

func (c *Controller) JoinCommands(token string) JoinCommands {
	addr := c.cfg.NodeAddr()
	fp := c.CAFingerprint()
	args := fmt.Sprintf("--controller %s --token %s --ca-fingerprint %s", addr, token, fp)
	return JoinCommands{
		Install:   fmt.Sprintf("git clone %s.git && cd QuorvexFusion && sudo ./install.sh worker %s --yes", RepoURL, args),
		Bootstrap: fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/NTFespolion307/QuorvexFusion/main/bootstrap.sh | sh -s -- %s", args),
		Docker:    fmt.Sprintf("docker run -d --name cluster-worker --restart unless-stopped -v cluster-worker:/var/lib/cluster-worker ghcr.io/ntfespolion307/cluster-worker %s", args),
		Direct:    "cluster worker " + args,
	}
}

func (c *Controller) RevokeJoinToken(id string) error {
	return c.store.RevokeJoinToken(id, time.Now())
}

// --- API tokens ---

func createAPIToken(st *store.Store, name string) (string, *store.APIToken, error) {
	token, id, hash := secret.New(secret.PrefixAPI)
	t := &store.APIToken{ID: id, Name: name, SecretHash: hash, CreatedAt: time.Now()}
	if err := st.CreateAPIToken(t); err != nil {
		return "", nil, err
	}
	return token, t, nil
}

func (c *Controller) CreateAPIToken(name string) (string, *store.APIToken, error) {
	return createAPIToken(c.store, name)
}

// --- nodes ---

var ErrAmbiguous = errors.New("ambiguous node name; use the node ID")

// FindNode resolves a node by ID, or by name if the name is unique.
func (c *Controller) FindNode(ref string) (*store.Node, error) {
	if n, err := c.store.GetNode(ref); err == nil {
		return n, nil
	}
	nodes, err := c.store.ListNodes()
	if err != nil {
		return nil, err
	}
	var match *store.Node
	for _, n := range nodes {
		if n.Name == ref {
			if match != nil {
				return nil, ErrAmbiguous
			}
			match = n
		}
	}
	if match == nil {
		return nil, store.ErrNotFound
	}
	return match, nil
}

// ApproveNode signs the stored CSR of a pending node. The worker picks up
// its certificate on its next poll (within ~10 seconds).
func (c *Controller) ApproveNode(id string) error {
	c.joinMu.Lock()
	defer c.joinMu.Unlock()
	n, err := c.store.GetNode(id)
	if err != nil {
		return err
	}
	if n.Status != store.NodePending {
		return fmt.Errorf("node %s is %s, not pending", id, n.Status)
	}
	if _, err := c.issueNodeCert(n.ID, n.CSR); err != nil {
		return err
	}
	c.log.Info("node approved", "node", id)
	c.events.publish("nodes")
	return nil
}

// RevokeNode blocks a node and immediately drops its connection.
func (c *Controller) RevokeNode(id string) error {
	if err := c.store.RevokeNode(id); err != nil {
		return err
	}
	c.hub.Disconnect(id, errRevoked)
	c.tm.mu.Lock()
	c.loseNodeAttemptsLocked(id, "node revoked")
	c.tm.mu.Unlock()
	c.log.Info("node revoked", "node", id)
	c.events.publish("nodes")
	return nil
}

// DeleteNode forgets a node entirely. Online nodes must be revoked first.
func (c *Controller) DeleteNode(id string) error {
	n, err := c.store.GetNode(id)
	if err != nil {
		return err
	}
	if n.Status == store.NodeApproved && c.hub.Get(id) != nil {
		return errors.New("node is online; revoke it first")
	}
	if err := c.store.DeleteNode(id); err != nil {
		return err
	}
	c.hub.Disconnect(id, errRevoked)
	c.events.publish("nodes")
	return nil
}

// --- views ---

// NodeView combines a node's stored record with its live session state.
type NodeView struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Status        string            `json:"status"` // pending | online | offline | revoked
	Location      string            `json:"location"`
	Ephemeral     bool              `json:"ephemeral"`
	Draining      bool              `json:"draining"`
	Labels        map[string]string `json:"labels"`
	Addr          string            `json:"addr"`
	RTTMillis     float64           `json:"rtt_ms"`
	SharedStorage string            `json:"shared_storage,omitempty"`
	Version       string            `json:"version,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	ConnectedAt   *time.Time        `json:"connected_at,omitempty"`
	LastSeen      *time.Time        `json:"last_seen,omitempty"`
	Hardware      *pb.HardwareInfo  `json:"hardware,omitempty"`
	Metrics       *pb.Metrics       `json:"metrics,omitempty"`
	Used          Resources         `json:"used"` // reserved by tasks on this node
}

func (c *Controller) nodeView(n *store.Node) *NodeView {
	v := &NodeView{
		ID: n.ID, Name: n.Name, Status: string(n.Status), Location: n.Location, Ephemeral: n.Ephemeral,
		Draining: n.Draining, Labels: n.EffectiveLabels(), Addr: n.Addr, SharedStorage: n.SharedStorage,
		CreatedAt: n.CreatedAt, LastSeen: n.LastSeen,
	}
	if n.Hardware != "" {
		hw := &pb.HardwareInfo{}
		if protojson.Unmarshal([]byte(n.Hardware), hw) == nil {
			v.Hardware = hw
		}
	}
	if n.Status == store.NodeApproved {
		v.Status = "offline"
		if s := c.hub.Get(n.ID); s != nil {
			snap := s.Snapshot()
			v.Status = "online"
			v.Addr = snap.RemoteAddr
			v.RTTMillis = float64(snap.RTT.Microseconds()) / 1000
			v.ConnectedAt = &snap.ConnectedAt
			v.LastSeen = &snap.LastSeen
			v.Metrics = snap.Metrics
			v.Hardware = snap.Hello.Hardware
			v.Version = snap.Hello.Version
		}
	}
	return v
}

func (c *Controller) ListNodeViews() ([]*NodeView, error) {
	nodes, err := c.store.ListNodes()
	if err != nil {
		return nil, err
	}
	used := c.usedResources()
	out := make([]*NodeView, 0, len(nodes))
	for _, n := range nodes {
		v := c.nodeView(n)
		if u := used[n.ID]; u != nil {
			v.Used = *u
		}
		out = append(out, v)
	}
	return out, nil
}

func (c *Controller) NodeView(id string) (*NodeView, error) {
	n, err := c.store.GetNode(id)
	if err != nil {
		return nil, err
	}
	v := c.nodeView(n)
	if u := c.usedResources()[id]; u != nil {
		v.Used = *u
	}
	return v, nil
}

// NodeHistory returns the node's recent metrics, oldest first.
func (c *Controller) NodeHistory(id string) []*pb.Metrics {
	if s := c.hub.Get(id); s != nil {
		return s.History()
	}
	return nil
}

// Resources is an amount of CPU, memory and GPUs.
type Resources struct {
	CPUs        float64 `json:"cpus"`
	MemoryBytes uint64  `json:"memory_bytes"`
	GPUs        int     `json:"gpus"`
}

// PoolSummary describes the whole cluster's capacity and usage.
type PoolSummary struct {
	NodesOnline  int                      `json:"nodes_online"`
	NodesOffline int                      `json:"nodes_offline"`
	NodesPending int                      `json:"nodes_pending"`
	Total        Resources                `json:"total"` // online nodes only
	Used         Resources                `json:"used"`  // reserved by running tasks
	ByLocation   map[string]*LocationPool `json:"by_location"`
	Locations    []string                 `json:"locations"`
	Tasks        store.TaskCounts         `json:"tasks"` // queued/assigned/running across all jobs
}

type LocationPool struct {
	Total Resources `json:"total"`
	Used  Resources `json:"used"`
}

func (r *Resources) add(o Resources) {
	r.CPUs += o.CPUs
	r.MemoryBytes += o.MemoryBytes
	r.GPUs += o.GPUs
}

func (c *Controller) Pool() (*PoolSummary, error) {
	views, err := c.ListNodeViews()
	if err != nil {
		return nil, err
	}
	p := &PoolSummary{ByLocation: map[string]*LocationPool{}}
	for _, v := range views {
		switch v.Status {
		case "pending":
			p.NodesPending++
		case "offline":
			p.NodesOffline++
		case "online":
			p.NodesOnline++
			if v.Hardware == nil {
				continue
			}
			total := Resources{CPUs: v.Hardware.CpuLimit, MemoryBytes: v.Hardware.MemoryBytes, GPUs: len(v.Hardware.Gpus)}
			p.Total.add(total)
			p.Used.add(v.Used)
			loc := v.Location
			if loc == "" {
				loc = "default"
			}
			l := p.ByLocation[loc]
			if l == nil {
				l = &LocationPool{}
				p.ByLocation[loc] = l
			}
			l.Total.add(total)
			l.Used.add(v.Used)
		}
	}
	for loc := range p.ByLocation {
		p.Locations = append(p.Locations, loc)
	}
	sort.Strings(p.Locations)
	if p.Tasks, err = c.store.GlobalTaskCounts(); err != nil {
		return nil, err
	}
	return p, nil
}

// normalizeLabelKey keeps label keys simple and predictable.
func normalizeLabelKey(k string) string { return strings.ToLower(strings.TrimSpace(k)) }

// SetNodeLabels replaces the admin-set labels of a node.
func (c *Controller) SetNodeLabels(id string, labels map[string]string) error {
	clean := map[string]string{}
	for k, v := range labels {
		if k = normalizeLabelKey(k); k != "" {
			clean[k] = strings.TrimSpace(v)
		}
	}
	if err := c.store.SetNodeLabels(id, clean); err != nil {
		return err
	}
	c.events.publish("nodes")
	return nil
}
