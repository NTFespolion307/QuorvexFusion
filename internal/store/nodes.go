package store

import (
	"database/sql"
	"errors"
	"time"
)

type NodeStatus string

const (
	NodePending  NodeStatus = "pending"
	NodeApproved NodeStatus = "approved"
	NodeRevoked  NodeStatus = "revoked"
)

// Node is a machine that has joined (or asked to join) the cluster. Live
// state such as "online" and current metrics is not stored here; it lives in
// the controller's in-memory session hub.
type Node struct {
	ID            string
	Name          string
	Status        NodeStatus
	PubKeyFP      string
	CSR           []byte
	CertSerial    string
	TokenID       string
	Location      string
	Ephemeral     bool
	WorkerLabels  map[string]string
	Labels        map[string]string
	Hardware      string // protojson-encoded HardwareInfo
	Addr          string
	SharedStorage string
	Draining      bool
	Network       string // "" = detect from the connection address, "local" or "remote"
	CreatedAt     time.Time
	ApprovedAt    *time.Time
	LastSeen      *time.Time
}

// EffectiveLabels merges worker-reported labels with admin labels (admin
// wins) and adds the built-in "location" and "hostname" labels, so a job
// can target one machine with --require hostname=NAME.
func (n *Node) EffectiveLabels() map[string]string {
	out := map[string]string{}
	for k, v := range n.WorkerLabels {
		out[k] = v
	}
	for k, v := range n.Labels {
		out[k] = v
	}
	if n.Location != "" {
		if _, ok := out["location"]; !ok {
			out["location"] = n.Location
		}
	}
	if _, ok := out["hostname"]; !ok && n.Name != "" {
		out["hostname"] = n.Name
	}
	return out
}

const nodeCols = `id, name, status, pubkey_fp, csr, cert_serial, token_id, location, ephemeral,
	worker_labels, labels, hardware, addr, shared_storage, draining, created_at, approved_at, last_seen, network`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var wl, l string
	var created int64
	var approved, seen sql.NullInt64
	err := row.Scan(&n.ID, &n.Name, &n.Status, &n.PubKeyFP, &n.CSR, &n.CertSerial, &n.TokenID, &n.Location,
		&n.Ephemeral, &wl, &l, &n.Hardware, &n.Addr, &n.SharedStorage, &n.Draining, &created, &approved, &seen, &n.Network)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.WorkerLabels = decodeLabels(wl)
	n.Labels = decodeLabels(l)
	n.CreatedAt = time.Unix(created, 0)
	n.ApprovedAt = fromNullTime(approved)
	n.LastSeen = fromNullTime(seen)
	return &n, nil
}

func (s *Store) CreateNode(n *Node) error {
	_, err := s.db.Exec(`INSERT INTO nodes
		(id, name, status, pubkey_fp, csr, cert_serial, token_id, location, ephemeral,
		 worker_labels, labels, created_at, approved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.Status, n.PubKeyFP, n.CSR, n.CertSerial, n.TokenID, n.Location, boolInt(n.Ephemeral),
		encodeLabels(n.WorkerLabels), encodeLabels(n.Labels), unix(n.CreatedAt), nullTime(n.ApprovedAt))
	return err
}

func (s *Store) GetNode(id string) (*Node, error) {
	return scanNode(s.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
}

func (s *Store) NodeByPubKey(fp string) (*Node, error) {
	return scanNode(s.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE pubkey_fp = ?`, fp))
}

func (s *Store) ListNodes() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ApproveNode marks a node approved with a freshly issued certificate serial.
func (s *Store) ApproveNode(id, certSerial string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE nodes SET status = ?, cert_serial = ?, approved_at = COALESCE(approved_at, ?), csr = NULL
		WHERE id = ? AND status != ?`, NodeApproved, certSerial, unix(now), id, NodeRevoked)
	return expectRow(res, err)
}

// RevokeNode permanently blocks a node. Its certificate stops working on the
// next connection check, and the controller drops any live connection.
func (s *Store) RevokeNode(id string) error {
	res, err := s.db.Exec(`UPDATE nodes SET status = ?, cert_serial = '', csr = NULL WHERE id = ?`, NodeRevoked, id)
	return expectRow(res, err)
}

func (s *Store) DeleteNode(id string) error {
	res, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	return expectRow(res, err)
}

// NodeConnectInfo is what a worker reports when its control stream opens.
type NodeConnectInfo struct {
	Name          string
	Hardware      string
	Location      string
	Ephemeral     bool
	WorkerLabels  map[string]string
	SharedStorage string
	Addr          string
}

// UpdateNodeOnConnect records the worker's self-reported details. An empty
// location keeps the existing one (e.g. set from the join token).
func (s *Store) UpdateNodeOnConnect(id string, info NodeConnectInfo, now time.Time) error {
	res, err := s.db.Exec(`UPDATE nodes SET
		name = ?, hardware = ?, location = CASE WHEN ? = '' THEN location ELSE ? END,
		ephemeral = (ephemeral OR ?), worker_labels = ?, shared_storage = ?, addr = ?, last_seen = ?
		WHERE id = ?`,
		info.Name, info.Hardware, info.Location, info.Location, boolInt(info.Ephemeral),
		encodeLabels(info.WorkerLabels), info.SharedStorage, info.Addr, unix(now), id)
	return expectRow(res, err)
}

func (s *Store) TouchNode(id string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE nodes SET last_seen = ? WHERE id = ?`, unix(now), id)
	return err
}

func (s *Store) SetNodeDraining(id string, draining bool) error {
	res, err := s.db.Exec(`UPDATE nodes SET draining = ? WHERE id = ?`, boolInt(draining), id)
	return expectRow(res, err)
}

func (s *Store) SetNodeLabels(id string, labels map[string]string) error {
	res, err := s.db.Exec(`UPDATE nodes SET labels = ? WHERE id = ?`, encodeLabels(labels), id)
	return expectRow(res, err)
}

// NodeSettings are the per-node settings an admin can change.
type NodeSettings struct {
	Location  string `json:"location"`
	Ephemeral bool   `json:"ephemeral"`
	Network   string `json:"network"` // "", "local" or "remote"
}

func (s *Store) SetNodeSettings(id string, ns NodeSettings) error {
	res, err := s.db.Exec(`UPDATE nodes SET location = ?, ephemeral = ?, network = ? WHERE id = ?`,
		ns.Location, boolInt(ns.Ephemeral), ns.Network, id)
	return expectRow(res, err)
}

func (s *Store) SetNodeLocation(id, location string) error {
	res, err := s.db.Exec(`UPDATE nodes SET location = ? WHERE id = ?`, location, id)
	return expectRow(res, err)
}
