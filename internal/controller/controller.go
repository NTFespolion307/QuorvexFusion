// Package controller is the cluster's master: it accepts worker connections,
// tracks nodes and their resources, and (from milestone 2) schedules tasks.
package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/NTFespolion307/QuorvexFusion/internal/blobstore"
	pb "github.com/NTFespolion307/QuorvexFusion/internal/clusterpb"
	"github.com/NTFespolion307/QuorvexFusion/internal/discovery"
	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
	"github.com/NTFespolion307/QuorvexFusion/internal/version"
)

type Controller struct {
	cfg    *Config
	log    *slog.Logger
	store  *store.Store
	ca     *pki.CA
	hub    *Hub
	events *events
	tm     *taskManager

	joinMu          sync.Mutex
	stopAdvertising func()
	history         poolHistory
	blobs           *blobstore.Store
	downloads       atomic.Int64 // completed input downloads by workers

	throttleMu sync.Mutex
	lastEvent  map[string]time.Time // last UI event per topic (see publishThrottled)
}

// New loads the CA and opens the database of an initialised data dir.
func New(cfg *Config, log *slog.Logger) (*Controller, error) {
	ca, err := pki.LoadCA(cfg.path("ca.crt"), cfg.path("ca.key"))
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	st, err := store.Open(cfg.path("cluster.db"))
	if err != nil {
		return nil, err
	}
	blobs, err := blobstore.Open(cfg.path("blobs"))
	if err != nil {
		st.Close()
		return nil, err
	}
	c := &Controller{
		cfg: cfg, log: log, store: st, ca: ca, hub: NewHub(), events: newEvents(),
		tm: newTaskManager(cfg.path("logs")), blobs: blobs, lastEvent: map[string]time.Time{},
	}
	if err := c.restoreReservations(); err != nil {
		st.Close()
		return nil, fmt.Errorf("restore running tasks: %w", err)
	}
	return c, nil
}

func (c *Controller) Config() *Config     { return c.cfg }
func (c *Controller) Store() *store.Store { return c.store }
func (c *Controller) Hub() *Hub           { return c.hub }

// CAFingerprint identifies this controller to workers.
func (c *Controller) CAFingerprint() string { return c.ca.Fingerprint() }

// CACertPEM is handed to CLI clients so they can pin the controller.
func (c *Controller) CACertPEM() []byte { return pki.CertPEM(c.ca.Cert) }

// serverNames lists the names/IPs put in the server certificate.
func (c *Controller) serverNames() []string {
	names := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil {
		names = append(names, h)
	}
	if c.cfg.PublicAddr != "" {
		names = append(names, c.cfg.PublicAddr)
	}
	if c.cfg.Domain != "" {
		names = append(names, c.cfg.Domain)
	}
	names = append(names, c.cfg.ExtraNames...)
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
			names = append(names, ipn.IP.String())
		}
	}
	return names
}

// Run serves the node port and the web/API port until ctx is cancelled.
// httpHandler is the web UI + REST API (built by package api).
func (c *Controller) Run(ctx context.Context, httpHandler http.Handler) error {
	defer c.store.Close()

	// The server certificate is re-issued on every start so it always
	// covers the machine's current addresses. Workers pin the CA, not this
	// certificate, so that is harmless.
	serverCert, err := c.ca.IssueServerCert(c.serverNames())
	if err != nil {
		return err
	}
	caPool := certPool(c.ca)

	nodeTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven, // required for all RPCs except Join; see nodeserver.go
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	ns := newNodeServer(c)
	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(nodeTLS)),
		grpc.UnaryInterceptor(ns.unaryAuth),
		grpc.StreamInterceptor(ns.streamAuth),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	pb.RegisterNodeServiceServer(grpcServer, ns)

	nodeLn, err := net.Listen("tcp", c.cfg.NodeListen)
	if err != nil {
		return fmt.Errorf("node listener: %w", err)
	}
	webTLS, acmeHTTP := c.webTLS(serverCert)
	httpServer := &http.Server{
		Addr:              c.cfg.HTTPListen,
		Handler:           httpHandler,
		TLSConfig:         webTLS,
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpLn, err := net.Listen("tcp", c.cfg.HTTPListen)
	if err != nil {
		nodeLn.Close()
		return fmt.Errorf("http listener: %w", err)
	}

	c.log.Info("controller started",
		"node_listen", c.cfg.NodeListen, "http_listen", c.cfg.HTTPListen,
		"ui", c.cfg.UIURL(), "ca_fingerprint", c.CAFingerprint())

	errc := make(chan error, 2)
	go func() { errc <- grpcServer.Serve(nodeLn) }()
	go func() { errc <- httpServer.ServeTLS(httpLn, "", "") }()
	if acmeHTTP != nil {
		go func() {
			if err := acmeHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				c.log.Error("Let's Encrypt HTTP challenge listener", "addr", acmeHTTP.Addr, "err", err)
			}
		}()
	}
	if !c.cfg.DisableMDNS {
		c.advertise()
	}
	go c.persistLastSeen(ctx)
	go c.schedulerLoop(ctx)
	go c.poolSampler(ctx)
	go c.maintenanceLoop(ctx)
	go c.ephemeralLoop(ctx)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	c.log.Info("controller shutting down")
	if c.stopAdvertising != nil {
		c.stopAdvertising()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	shutdownQuietly(acmeHTTP)
	grpcServer.Stop() // control streams are long-lived; don't wait for them
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
		err = nil
	}
	return err
}

// advertise announces the controller on the LAN via mDNS. Failure is not
// fatal: workers can always be given the address directly.
func (c *Controller) advertise() {
	_, portStr, _ := net.SplitHostPort(c.cfg.NodeListen)
	port, _ := strconv.Atoi(portStr)
	ips := c.cfg.advertiseIPs()
	if port == 0 || len(ips) == 0 || ips[0].IsLoopback() {
		return
	}
	stop, err := discovery.Advertise(discovery.Advertisement{
		Port: port, IPs: ips, Fingerprint: c.CAFingerprint(), UIURL: c.cfg.UIURL(), Version: version.Version,
	})
	if err != nil {
		c.log.Warn("LAN discovery (mDNS) unavailable", "err", err)
		return
	}
	c.stopAdvertising = stop
	c.log.Info("advertising on the LAN via mDNS", "port", port)
}

// persistLastSeen records last_seen for online nodes once a minute so the
// UI can show "last seen" after a controller restart.
func (c *Controller) persistLastSeen(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, s := range c.hub.All() {
				_ = c.store.TouchNode(s.NodeID, time.Now())
			}
		}
	}
}
