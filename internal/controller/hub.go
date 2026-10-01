package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
)

// metricsHistory is how many metrics samples are kept per node for charts
// (one hour at the default 5 second interval).
const metricsHistory = 720

var (
	errReplaced  = errors.New("replaced by a newer connection from the same node")
	errRevoked   = errors.New("node revoked")
	errHeartbeat = errors.New("heartbeat timeout")
)

// Session is one live control stream from a worker. A node is "online"
// exactly when it has a Session in the Hub.
type Session struct {
	NodeID      string
	RemoteAddr  string
	ConnectedAt time.Time

	ctx    context.Context
	cancel context.CancelCauseFunc
	send   chan *pb.ControllerMessage

	mu       sync.Mutex
	hello    *pb.Hello
	metrics  *pb.Metrics
	history  []*pb.Metrics
	lastSeen time.Time
	rtt      time.Duration
	pings    map[int64]time.Time
}

func newSession(ctx context.Context, nodeID, remote string, hello *pb.Hello) *Session {
	ctx, cancel := context.WithCancelCause(ctx)
	now := time.Now()
	return &Session{
		NodeID: nodeID, RemoteAddr: remote, ConnectedAt: now,
		ctx: ctx, cancel: cancel,
		send:     make(chan *pb.ControllerMessage, 256),
		hello:    hello,
		lastSeen: now,
		pings:    map[int64]time.Time{},
	}
}

// Send queues a message for the worker. It returns false if the session
// has ended (the message is then dropped; the caller's state machine must
// cope, e.g. by requeueing work when the node goes offline).
func (s *Session) Send(msg *pb.ControllerMessage) bool {
	select {
	case s.send <- msg:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// Close ends the session; the stream handler returns soon after.
func (s *Session) Close(cause error) { s.cancel(cause) }

func (s *Session) touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *Session) sinceLastSeen() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastSeen)
}

func (s *Session) recordMetrics(m *pb.Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = m
	if len(s.history) >= metricsHistory {
		// Drop the oldest sample; copy keeps the backing array from growing.
		copy(s.history, s.history[1:])
		s.history = s.history[:len(s.history)-1]
	}
	s.history = append(s.history, m)
}

func (s *Session) newPing() *pb.Ping {
	s.mu.Lock()
	defer s.mu.Unlock()
	nonce := time.Now().UnixNano()
	s.pings[nonce] = time.Now()
	// Forget pings that never got an answer.
	for n, t := range s.pings {
		if time.Since(t) > time.Minute {
			delete(s.pings, n)
		}
	}
	return &pb.Ping{Nonce: nonce}
}

func (s *Session) recordPong(p *pb.Pong) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sent, ok := s.pings[p.Nonce]; ok {
		s.rtt = time.Since(sent)
		delete(s.pings, p.Nonce)
	}
}

// SessionSnapshot is a consistent copy of a session's live state.
type SessionSnapshot struct {
	RemoteAddr  string
	ConnectedAt time.Time
	LastSeen    time.Time
	RTT         time.Duration
	Hello       *pb.Hello
	Metrics     *pb.Metrics
}

func (s *Session) Snapshot() SessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionSnapshot{
		RemoteAddr: s.RemoteAddr, ConnectedAt: s.ConnectedAt, LastSeen: s.lastSeen,
		RTT: s.rtt, Hello: s.hello, Metrics: s.metrics,
	}
}

// History returns the retained metrics samples, oldest first.
func (s *Session) History() []*pb.Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.Metrics(nil), s.history...)
}

// Hub tracks the live session of every online node.
type Hub struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewHub() *Hub { return &Hub{sessions: map[string]*Session{}} }

// Register makes s the node's current session, closing any older one (a
// worker that reconnects after a network blip may race its old stream).
func (h *Hub) Register(s *Session) {
	h.mu.Lock()
	old := h.sessions[s.NodeID]
	h.sessions[s.NodeID] = s
	h.mu.Unlock()
	if old != nil {
		old.Close(errReplaced)
	}
}

// Unregister removes s if it is still the node's current session, and
// reports whether it was (i.e. whether the node is now offline).
func (h *Hub) Unregister(s *Session) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[s.NodeID] == s {
		delete(h.sessions, s.NodeID)
		return true
	}
	return false
}

func (h *Hub) Get(nodeID string) *Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessions[nodeID]
}

func (h *Hub) All() []*Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	return out
}

// Disconnect closes the node's session, if any.
func (h *Hub) Disconnect(nodeID string, cause error) {
	if s := h.Get(nodeID); s != nil {
		s.Close(cause)
	}
}
