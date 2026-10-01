package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
)

// These tests run a real controller and real workers in one process,
// talking gRPC over mutual TLS on localhost.

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type testCluster struct {
	c         *controller.Controller
	nodeAddr  string
	fp        string
	joinToken string
}

func startController(t *testing.T, heartbeatSec int) *testCluster {
	t.Helper()
	cfg := controller.DefaultConfig(filepath.Join(t.TempDir(), "ctl"))
	cfg.NodeListen = freePort(t)
	cfg.HTTPListen = freePort(t)
	cfg.MetricsIntervalSec = 1
	cfg.HeartbeatTimeoutSec = heartbeatSec
	res, err := controller.Init(cfg, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := controller.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := c.Run(ctx, http.NotFoundHandler()); err != nil {
			t.Errorf("controller: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 5*time.Second, "controller listening", func() bool {
		conn, err := net.Dial("tcp", cfg.NodeListen)
		if err == nil {
			conn.Close()
		}
		return err == nil
	})
	return &testCluster{c: c, nodeAddr: cfg.NodeListen, fp: res.CAFingerprint, joinToken: res.JoinToken}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (tc *testCluster) newWorker(t *testing.T, token, fp string) *Worker {
	return New(Options{
		DataDir: filepath.Join(t.TempDir(), "w"), Controller: tc.nodeAddr, Token: token, CAFingerprint: fp,
		Location: "test", Labels: map[string]string{"role": "e2e"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func runWorker(w *Worker) (cancel func(), result <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- w.Run(ctx) }()
	return cancel, ch
}

func nodeStatus(tc *testCluster, id string) string {
	v, err := tc.c.NodeView(id)
	if err != nil {
		return ""
	}
	return v.Status
}

func TestJoinConnectMetricsRevoke(t *testing.T) {
	tc := startController(t, 15)
	w := tc.newWorker(t, tc.joinToken, tc.fp)
	cancel, result := runWorker(w)
	defer cancel()

	var nodeID string
	waitFor(t, 10*time.Second, "node online", func() bool {
		st, err := w.id.loadState()
		if err != nil {
			return false
		}
		nodeID = st.NodeID
		return nodeStatus(tc, nodeID) == "online"
	})

	v, _ := tc.c.NodeView(nodeID)
	if v.Hardware == nil || v.Hardware.LogicalCores < 1 {
		t.Errorf("no hardware reported: %+v", v.Hardware)
	}
	if v.Location != "test" || v.Labels["role"] != "e2e" {
		t.Errorf("location/labels not recorded: %q %v", v.Location, v.Labels)
	}
	waitFor(t, 5*time.Second, "metrics", func() bool { return len(tc.c.NodeHistory(nodeID)) >= 2 })

	pool, _ := tc.c.Pool()
	if pool.NodesOnline != 1 || pool.Total.CPUs < 1 {
		t.Errorf("pool = %+v", pool)
	}

	// Revoking disconnects immediately and the worker gives up for good.
	if err := tc.c.RevokeNode(nodeID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrRevoked) {
			t.Fatalf("worker exited with %v, want ErrRevoked", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop after revocation")
	}
	if s := nodeStatus(tc, nodeID); s != "revoked" {
		t.Errorf("status after revoke = %q", s)
	}
}

func TestManualApproval(t *testing.T) {
	pendingPollInterval = 200 * time.Millisecond
	defer func() { pendingPollInterval = 10 * time.Second }()

	tc := startController(t, 15)
	issued, _, err := tc.c.CreateJoinToken(controller.JoinTokenOptions{AutoApprove: false})
	if err != nil {
		t.Fatal(err)
	}
	// A short join code, no fingerprint: the code's pin verifies the CA.
	w := tc.newWorker(t, strings.ToLower(issued.Code), "")
	nodeID, pending, err := w.JoinOnly(context.Background())
	if err != nil || !pending {
		t.Fatalf("JoinOnly: id=%s pending=%v err=%v", nodeID, pending, err)
	}
	if s := nodeStatus(tc, nodeID); s != "pending" {
		t.Fatalf("status = %q, want pending", s)
	}

	// The service starts while still pending (no token needed any more).
	w.opts.Token = ""
	cancel, _ := runWorker(w)
	defer cancel()
	time.Sleep(500 * time.Millisecond)
	if s := nodeStatus(tc, nodeID); s != "pending" {
		t.Fatalf("status = %q before approval", s)
	}
	if err := tc.c.ApproveNode(nodeID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "approved node online", func() bool { return nodeStatus(tc, nodeID) == "online" })
}

func TestWrongFingerprintAndBadToken(t *testing.T) {
	tc := startController(t, 15)
	w := tc.newWorker(t, tc.joinToken, "sha256:00")
	if _, _, err := w.JoinOnly(context.Background()); err == nil {
		t.Fatal("joined despite a wrong CA fingerprint")
	}
	w = tc.newWorker(t, "cjt_abcd_wrong", tc.fp)
	if _, _, err := w.JoinOnly(context.Background()); err == nil {
		t.Fatal("joined with a bogus token")
	}
}

func TestJoinCodes(t *testing.T) {
	tc := startController(t, 15)
	other := startController(t, 15) // a second, unrelated controller
	issued, _, err := tc.c.CreateJoinToken(controller.JoinTokenOptions{AutoApprove: true})
	if err != nil {
		t.Fatal(err)
	}

	// A valid code joins without any fingerprint.
	w := tc.newWorker(t, issued.Code, "")
	if _, pending, err := w.JoinOnly(context.Background()); err != nil || pending {
		t.Fatalf("join with code: pending=%v err=%v", pending, err)
	}

	// The same code pointed at another controller is refused by the worker
	// before it sends anything (the pin doesn't match that CA).
	w = New(Options{DataDir: filepath.Join(t.TempDir(), "w"), Controller: other.nodeAddr, Token: issued.Code,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, _, err := w.JoinOnly(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("code accepted by the wrong controller: %v", err)
	}

	// A typo in the secret half: the pin matches, the controller says no.
	code := []byte(strings.ReplaceAll(issued.Code, "-", ""))
	if code[19] == 'A' {
		code[19] = 'B'
	} else {
		code[19] = 'A'
	}
	w = tc.newWorker(t, string(code), "")
	if _, _, err := w.JoinOnly(context.Background()); err == nil {
		t.Fatal("joined with a mistyped code")
	}
}

// A node that stops sending anything is marked offline after the heartbeat
// timeout, even though its TCP connection stays open.
func TestHeartbeatTimeout(t *testing.T) {
	tc := startController(t, 3)
	w := tc.newWorker(t, tc.joinToken, tc.fp)
	nodeID, _, err := w.JoinOnly(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Hand-rolled client: sends Hello, then goes silent.
	ca, _ := w.id.ca()
	cert, _ := w.id.tlsCert()
	conn, err := grpc.NewClient(tc.nodeAddr, grpc.WithTransportCredentials(credentials.NewTLS(pki.PinnedClientTLS(ca, cert))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := pb.NewNodeServiceClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&pb.WorkerMessage{Msg: &pb.WorkerMessage_Hello{Hello: &pb.Hello{
		Hardware: &pb.HardwareInfo{Hostname: "silent", LogicalCores: 1, CpuLimit: 1},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "silent node online", func() bool { return nodeStatus(tc, nodeID) == "online" })
	start := time.Now()
	waitFor(t, 10*time.Second, "silent node offline", func() bool { return nodeStatus(tc, nodeID) == "offline" })
	if d := time.Since(start); d < 2*time.Second {
		t.Errorf("went offline after %v, before the 3s timeout", d)
	}
}
