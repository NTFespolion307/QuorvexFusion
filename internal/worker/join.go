package worker

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
)

// ErrRevoked means the controller permanently refused this node. The worker
// exits rather than retrying forever.
var ErrRevoked = errors.New("this node was revoked or removed by the controller; " +
	"delete the worker data directory and join again with a new token")

// ErrPending is returned by Join when the node awaits admin approval.
var ErrPending = errors.New("join is pending approval")

// trustController establishes which CA we trust for addr: an already
// pinned ca.crt, or the controller's CA after checking its fingerprint
// (given up-front, or confirmed interactively by the user).
func (w *Worker) trustController(addr string) (*x509.Certificate, error) {
	if ca, err := w.id.ca(); err == nil {
		if w.opts.CAFingerprint != "" && pki.Fingerprint(ca) != pki.NormalizeFingerprint(w.opts.CAFingerprint) {
			return nil, fmt.Errorf("pinned CA in %s does not match --ca-fingerprint", w.id.dir)
		}
		return ca, nil
	}
	ca, err := pki.FetchCA(addr, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("contact controller %s: %w", addr, err)
	}
	fp := pki.Fingerprint(ca)
	switch {
	case w.opts.CAFingerprint != "":
		if fp != pki.NormalizeFingerprint(w.opts.CAFingerprint) {
			return nil, fmt.Errorf("controller CA fingerprint mismatch!\n  expected %s\n  got      %s\n"+
				"Refusing to join: this may not be your controller", pki.NormalizeFingerprint(w.opts.CAFingerprint), fp)
		}
	case w.opts.ConfirmFingerprint != nil:
		if !w.opts.ConfirmFingerprint(fp) {
			return nil, errors.New("controller fingerprint not confirmed")
		}
	default:
		return nil, errors.New("no --ca-fingerprint given; pass it (shown by the controller with the join token) or use --yes to trust on first use")
	}
	if err := os.MkdirAll(w.id.dir, 0o700); err != nil {
		return nil, err
	}
	if err := pki.WriteCertPEM(w.id.path("ca.crt"), ca.Raw); err != nil {
		return nil, err
	}
	return ca, nil
}

// join performs (or polls) the join handshake. It returns nil once the node
// has a certificate, or ErrPending if an admin still has to approve it.
func (w *Worker) join(ctx context.Context, addr string) error {
	ca, err := w.trustController(addr)
	if err != nil {
		return err
	}
	key, err := w.id.key()
	if err != nil {
		return err
	}
	csr, err := pki.CreateCSR(key, w.hostname())
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(pki.PinnedClientTLS(ca, nil))))
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := pb.NewNodeServiceClient(conn).Join(ctx, &pb.JoinRequest{
		Token: w.opts.Token, CsrDer: csr, Hostname: w.hostname(),
		Location: w.opts.Location, Ephemeral: w.opts.Ephemeral,
	})
	if err != nil {
		if s, ok := status.FromError(err); ok {
			switch s.Code() {
			case codes.PermissionDenied:
				return fmt.Errorf("%w (%s)", ErrRevoked, s.Message())
			case codes.Unauthenticated:
				return fmt.Errorf("join refused: %s", s.Message())
			}
		}
		return err
	}

	if err := w.id.saveState(&state{Controller: addr, NodeID: resp.NodeId}); err != nil {
		return err
	}
	if resp.Status == pb.JoinResponse_PENDING {
		return ErrPending
	}
	cert, err := x509.ParseCertificate(resp.CertDer)
	if err != nil {
		return fmt.Errorf("controller sent a bad certificate: %w", err)
	}
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return fmt.Errorf("controller certificate not signed by the pinned CA: %w", err)
	}
	if err := pki.WriteCertPEM(w.id.path("node.crt"), cert.Raw); err != nil {
		return err
	}
	w.log.Info("joined cluster", "node", resp.NodeId, "controller", addr)
	return nil
}

// JoinOnly runs the join step and returns, reporting the node ID and
// whether approval is still pending. Used by `cluster worker join` and
// install.sh, so the service starts with the identity already in place.
func (w *Worker) JoinOnly(ctx context.Context) (nodeID string, pending bool, err error) {
	addr, err := w.controllerAddr()
	if err != nil {
		return "", false, err
	}
	if w.id.hasCert() {
		st, _ := w.id.loadState()
		if st != nil {
			return st.NodeID, false, nil
		}
	}
	err = w.join(ctx, addr)
	st, _ := w.id.loadState()
	if st != nil {
		nodeID = st.NodeID
	}
	if errors.Is(err, ErrPending) {
		return nodeID, true, nil
	}
	return nodeID, false, err
}
