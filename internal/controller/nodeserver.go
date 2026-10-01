package controller

import (
	"context"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
	"github.com/NTFespolion307/QuorvexFusion/internal/ratelimit"
	"github.com/NTFespolion307/QuorvexFusion/internal/secret"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// nodeServer implements the gRPC NodeService that workers talk to.
type nodeServer struct {
	pb.UnimplementedNodeServiceServer
	c           *Controller
	joinLimiter *ratelimit.Limiter
}

func newNodeServer(c *Controller) *nodeServer {
	return &nodeServer{
		c: c,
		// 10 join attempts at once per IP, then one every 6 seconds. A
		// pending worker polls every 10s, which stays under this.
		joinLimiter: ratelimit.New(6*time.Second, 10),
	}
}

// --- authentication ---
//
// The TLS listener requests but does not require a client certificate
// (Join must work without one). These interceptors enforce that every other
// RPC presents a valid, current node certificate.

type nodeIDKey struct{}

func nodeIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(nodeIDKey{}).(string)
	return id
}

func (ns *nodeServer) authenticate(ctx context.Context) (context.Context, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
		return nil, status.Error(codes.Unauthenticated, "node certificate required")
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]
	node, err := ns.c.store.GetNode(leaf.Subject.CommonName)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, "unknown node; it may have been removed")
	}
	// The serial check means a revoked node's certificate stops working
	// immediately, and re-issuing a certificate invalidates the old one.
	if node.Status != store.NodeApproved || node.CertSerial != pki.SerialString(leaf) {
		return nil, status.Error(codes.PermissionDenied, "node is revoked or its certificate was replaced")
	}
	return context.WithValue(ctx, nodeIDKey{}, node.ID), nil
}

func (ns *nodeServer) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if info.FullMethod == pb.NodeService_Join_FullMethodName {
		return h(ctx, req)
	}
	ctx, err := ns.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return h(ctx, req)
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

func (ns *nodeServer) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	ctx, err := ns.authenticate(ss.Context())
	if err != nil {
		return err
	}
	return h(srv, &authedStream{ServerStream: ss, ctx: ctx})
}

func peerIP(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}

// --- Join ---

func (ns *nodeServer) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	ip := peerIP(ctx)
	if !ns.joinLimiter.Allow(ip) {
		return nil, status.Error(codes.ResourceExhausted, "too many join attempts; slow down")
	}
	csr, err := pki.ParseCSR(req.CsrDer)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	fp, err := pki.PublicKeyFingerprint(csr.PublicKey)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	c := ns.c
	c.joinMu.Lock() // serialise joins so two calls with one key can't both create a node
	defer c.joinMu.Unlock()

	// A key we already know: this is a pending node polling for approval,
	// or an approved node re-fetching its certificate. The CSR signature
	// proves possession of the key, so no token is needed.
	if node, err := c.store.NodeByPubKey(fp); err == nil {
		switch node.Status {
		case store.NodeRevoked:
			return nil, status.Error(codes.PermissionDenied, "this node has been revoked")
		case store.NodePending:
			return &pb.JoinResponse{Status: pb.JoinResponse_PENDING, NodeId: node.ID, CaCertDer: c.ca.Cert.Raw}, nil
		default:
			return c.issueNodeCert(node.ID, csr.Raw)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// A new node: the token must be valid.
	tok, err := c.store.UseJoinToken(req.Token, time.Now())
	if err != nil {
		c.log.Warn("join rejected", "ip", ip, "hostname", req.Hostname, "err", err)
		if errors.Is(err, store.ErrBadToken) {
			return nil, status.Error(codes.Unauthenticated, "invalid join token")
		}
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	node := &store.Node{
		ID:        "n" + secret.RandomHex(4),
		Name:      req.Hostname,
		Status:    store.NodePending,
		PubKeyFP:  fp,
		CSR:       csr.Raw,
		TokenID:   tok.ID,
		Location:  req.Location,
		Ephemeral: req.Ephemeral || tok.Ephemeral,
		CreatedAt: time.Now(),
	}
	if node.Name == "" {
		node.Name = node.ID
	}
	if node.Location == "" {
		node.Location = tok.Location
	}
	if err := c.store.CreateNode(node); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	c.log.Info("node joined", "node", node.ID, "name", node.Name, "ip", ip, "token", tok.ID, "auto_approve", tok.AutoApprove)
	c.events.publish("nodes")
	if !tok.AutoApprove {
		return &pb.JoinResponse{Status: pb.JoinResponse_PENDING, NodeId: node.ID, CaCertDer: c.ca.Cert.Raw}, nil
	}
	return c.issueNodeCert(node.ID, csr.Raw)
}

// issueNodeCert signs csrDER for nodeID and marks the node approved.
func (c *Controller) issueNodeCert(nodeID string, csrDER []byte) (*pb.JoinResponse, error) {
	csr, err := pki.ParseCSR(csrDER)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	cert, err := c.ca.SignNodeCSR(csr, nodeID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := c.store.ApproveNode(nodeID, pki.SerialString(cert), time.Now()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.JoinResponse{
		Status: pb.JoinResponse_APPROVED, NodeId: nodeID,
		CertDer: cert.Raw, CaCertDer: c.ca.Cert.Raw,
	}, nil
}

// --- Connect: the control stream ---

func (ns *nodeServer) Connect(stream pb.NodeService_ConnectServer) error {
	c := ns.c
	nodeID := nodeIDFrom(stream.Context())

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.Hardware == nil {
		return status.Error(codes.InvalidArgument, "first message must be a Hello with hardware info")
	}

	remote := ""
	if p, ok := peer.FromContext(stream.Context()); ok {
		remote = p.Addr.String()
	}
	hwJSON, _ := protojson.Marshal(hello.Hardware)
	if err := c.store.UpdateNodeOnConnect(nodeID, store.NodeConnectInfo{
		Name: hello.Hardware.Hostname, Hardware: string(hwJSON), Location: hello.Location,
		Ephemeral: hello.Ephemeral, WorkerLabels: hello.Labels, SharedStorage: hello.SharedStorage,
		Addr: remote,
	}, time.Now()); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	sess := newSession(stream.Context(), nodeID, remote, hello)
	c.hub.Register(sess)
	c.log.Info("node online", "node", nodeID, "name", hello.Hardware.Hostname, "addr", remote, "version", hello.Version)
	c.events.publish("nodes")

	sess.Send(&pb.ControllerMessage{Msg: &pb.ControllerMessage_Welcome{Welcome: &pb.Welcome{
		NodeId: nodeID, MetricsIntervalSeconds: int32(c.cfg.MetricsIntervalSec),
	}}})

	// Sender: the only goroutine that calls stream.Send (gRPC streams do not
	// allow concurrent sends).
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		for {
			select {
			case msg := <-sess.send:
				if err := stream.Send(msg); err != nil {
					sess.Close(err)
					return
				}
			case <-sess.ctx.Done():
				return
			}
		}
	}()

	// Receiver: every message counts as a heartbeat.
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				sess.Close(err)
				return
			}
			sess.touch()
			switch m := msg.Msg.(type) {
			case *pb.WorkerMessage_Metrics:
				sess.recordMetrics(m.Metrics)
			case *pb.WorkerMessage_Pong:
				sess.recordPong(m.Pong)
			}
		}
	}()

	// Watchdog: ping regularly (for latency) and drop silent nodes.
	go func() {
		t := time.NewTicker(min(5*time.Second, c.cfg.HeartbeatTimeout()/3))
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if sess.sinceLastSeen() > c.cfg.HeartbeatTimeout() {
					sess.Close(errHeartbeat)
					return
				}
				sess.Send(&pb.ControllerMessage{Msg: &pb.ControllerMessage_Ping{Ping: sess.newPing()}})
			case <-sess.ctx.Done():
				return
			}
		}
	}()

	<-sess.ctx.Done()
	cause := context.Cause(sess.ctx)
	select {
	case <-senderDone:
	case <-time.After(5 * time.Second):
	}

	if c.hub.Unregister(sess) {
		_ = c.store.TouchNode(nodeID, time.Now())
		c.log.Info("node offline", "node", nodeID, "reason", cause)
		c.events.publish("nodes")
	}
	if errors.Is(cause, errRevoked) {
		return status.Error(codes.PermissionDenied, "node revoked")
	}
	if errors.Is(cause, errReplaced) {
		return status.Error(codes.Aborted, cause.Error())
	}
	return nil
}
