// Package worker is the agent that runs on every machine in the pool. It
// joins the cluster, keeps one outbound control stream to the controller,
// reports hardware and metrics, and (from milestone 2) runs tasks.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/user"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/hw"
	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
	"github.com/NTFespolion307/QuorvexFusion/internal/runner"
	"github.com/NTFespolion307/QuorvexFusion/internal/version"
)

// pendingPollInterval is how often a node awaiting approval asks again.
// A variable so tests can shorten it.
var pendingPollInterval = 10 * time.Second

type Options struct {
	DataDir       string
	Controller    string // host:port; may be empty once joined
	Token         string // only needed for the first join
	CAFingerprint string // expected controller CA fingerprint
	// ConfirmFingerprint is asked to approve an unknown controller CA when
	// no CAFingerprint is given (interactive prompt, or --yes).
	ConfirmFingerprint func(fp string) bool

	// TaskUser runs tasks as this user ("" = automatic: "cluster" if the
	// worker is root and that user exists, else the worker's own user).
	TaskUser string

	Name          string // overrides the reported hostname
	Location      string
	Ephemeral     bool
	Labels        map[string]string
	SharedStorage string

	Log *slog.Logger
}

type Worker struct {
	opts   Options
	id     identity
	log    *slog.Logger
	runner *runner.Runner
}

func New(opts Options) *Worker {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Worker{opts: opts, id: identity{dir: opts.DataDir}, log: opts.Log}
}

func (w *Worker) hostname() string {
	if w.opts.Name != "" {
		return w.opts.Name
	}
	h, _ := os.Hostname()
	return h
}

// controllerAddr prefers the address given on the command line, falling
// back to the one saved at join time.
func (w *Worker) controllerAddr() (string, error) {
	if w.opts.Controller != "" {
		return WithDefaultPort(w.opts.Controller), nil
	}
	if st, err := w.id.loadState(); err == nil && st.Controller != "" {
		return st.Controller, nil
	}
	return "", errors.New("no controller address: pass --controller host:port")
}

// WithDefaultPort adds the default node port (7443) to a bare host name.
func WithDefaultPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(strings.Trim(addr, "[]"), "7443")
	}
	return addr
}

// Run joins if necessary, then keeps the control stream up until ctx is
// cancelled. It returns ErrRevoked if the controller rejects the node.
func (w *Worker) Run(ctx context.Context) error {
	addr, err := w.controllerAddr()
	if err != nil {
		return err
	}

	// Join, or wait for approval of an earlier join.
	for !w.id.hasCert() {
		if _, err := w.id.loadState(); err != nil && w.opts.Token == "" {
			return errors.New("this worker has not joined yet: pass --token")
		}
		err := w.join(ctx, addr)
		if err == nil {
			break
		}
		if errors.Is(err, ErrRevoked) {
			return err
		}
		if errors.Is(err, ErrPending) {
			st, _ := w.id.loadState()
			w.log.Info("waiting for an admin to approve this node in the web UI or with `cluster nodes approve`",
				"node", st.NodeID)
		} else {
			w.log.Warn("join failed; retrying", "err", err)
		}
		if !sleep(ctx, pendingPollInterval) {
			return nil
		}
	}

	if err := w.startRunner(); err != nil {
		return err
	}
	defer w.runner.Shutdown()

	ca, err := w.id.ca()
	if err != nil {
		return err
	}
	// A data dir from an earlier join to a different controller would
	// otherwise fail TLS forever with a confusing error.
	if fp := w.opts.CAFingerprint; fp != "" && pki.Fingerprint(ca) != pki.NormalizeFingerprint(fp) {
		return fmt.Errorf("%s holds the identity of a node of a different controller (CA %s, but --ca-fingerprint is %s); "+
			"remove that directory to join this controller", w.id.dir, pki.Fingerprint(ca), pki.NormalizeFingerprint(fp))
	}
	cert, err := w.id.tlsCert()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(pki.PinnedClientTLS(ca, cert))),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 15 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewNodeServiceClient(conn)

	// Reconnect forever with exponential backoff (1s .. 60s, with jitter so
	// a controller restart isn't hit by every worker at the same instant).
	backoff := time.Second
	for {
		welcomed, err := w.session(ctx, client)
		if ctx.Err() != nil {
			return nil
		}
		if s, ok := status.FromError(err); ok &&
			(s.Code() == codes.PermissionDenied || s.Code() == codes.Unauthenticated) {
			return fmt.Errorf("%w (%s)", ErrRevoked, s.Message())
		}
		if welcomed {
			backoff = time.Second
		}
		delay := backoff/2 + rand.N(backoff)
		w.log.Warn("disconnected from controller; reconnecting", "err", err, "in", delay.Round(100*time.Millisecond))
		if !sleep(ctx, delay) {
			return nil
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

// startRunner prepares task execution: which user tasks run as, and
// whether systemd can enforce CPU/memory limits (needs root).
func (w *Worker) startRunner() error {
	taskUser := w.opts.TaskUser
	if taskUser == "" && os.Geteuid() == 0 {
		if _, err := user.Lookup("cluster"); err == nil {
			taskUser = "cluster"
		} else {
			w.log.Warn("tasks will run as root: create a 'cluster' user or pass --task-user")
		}
	}
	useSystemd := hw.HasSystemd() && os.Geteuid() == 0
	if !useSystemd {
		w.log.Info("systemd-run not usable (needs systemd and root); task CPU/memory limits are not enforced")
	}
	r, err := runner.New(runner.Options{
		Dir: w.id.path("tasks"), TaskUser: taskUser, UseSystemd: useSystemd, Log: w.log,
	})
	if err != nil {
		return err
	}
	w.runner = r
	return nil
}

// session runs one control stream until it fails. welcomed reports whether
// the controller accepted us (used to reset the backoff).
func (w *Worker) session(ctx context.Context, client pb.NodeServiceClient) (welcomed bool, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// Hardware is re-probed on every connection so changes (e.g. Docker
	// installed later) are picked up after a reconnect.
	info := hw.Probe()
	if w.opts.Name != "" {
		info.Hostname = w.opts.Name
	}

	stream, err := client.Connect(ctx)
	if err != nil {
		return false, err
	}
	err = stream.Send(&pb.WorkerMessage{Msg: &pb.WorkerMessage_Hello{Hello: &pb.Hello{
		Version: version.Version, Hardware: info, Location: w.opts.Location,
		Ephemeral: w.opts.Ephemeral, Labels: w.opts.Labels, SharedStorage: w.opts.SharedStorage,
		RunningAttempts: w.runner.Held(),
	}}})
	if err != nil {
		return false, err
	}
	first, err := stream.Recv()
	if err != nil {
		return false, err
	}
	welcome := first.GetWelcome()
	if welcome == nil {
		return false, errors.New("controller did not send Welcome")
	}
	w.log.Info("connected to controller", "node", welcome.NodeId,
		"cpus", info.CpuLimit, "memory_gb", info.MemoryBytes>>30, "gpus", len(info.Gpus))

	// All sends go through this channel to a single goroutine, because a
	// gRPC stream does not allow concurrent Send calls.
	out := make(chan *pb.WorkerMessage, 64)
	go func() {
		for {
			select {
			case msg := <-out:
				if err := stream.Send(msg); err != nil {
					cancel(err)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	send := func(m *pb.WorkerMessage) bool {
		select {
		case out <- m:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// The runner resends logs and results from where the controller is.
	w.runner.Connected(send, welcome.LogOffsets)
	defer w.runner.Disconnected()

	// Metrics loop; doubles as the heartbeat.
	interval := time.Duration(welcome.MetricsIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	sampler := hw.NewSampler(info)
	sampler.Sample() // prime the rate counters
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				send(&pb.WorkerMessage{Msg: &pb.WorkerMessage_Metrics{Metrics: sampler.Sample()}})
			case <-ctx.Done():
				return
			}
		}
	}()

	// Receive loop.
	for {
		msg, err := stream.Recv()
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				err = cause
			}
			return true, err
		}
		switch m := msg.Msg.(type) {
		case *pb.ControllerMessage_Ping:
			send(&pb.WorkerMessage{Msg: &pb.WorkerMessage_Pong{Pong: &pb.Pong{Nonce: m.Ping.Nonce}}})
		case *pb.ControllerMessage_Assign:
			w.runner.Start(m.Assign)
		case *pb.ControllerMessage_Cancel:
			w.runner.Cancel(m.Cancel.AttemptId, m.Cancel.Reason)
		case *pb.ControllerMessage_ResultAck:
			w.runner.Ack(m.ResultAck.AttemptId)
		}
	}
}

// sleep waits for d or until ctx is done; it reports false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
