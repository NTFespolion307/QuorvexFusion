package controller

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// webTLS builds the TLS config of the web UI / API port.
//
// Without a domain it serves the certificate from the cluster's own CA.
// With Config.Domain, browsers asking for that name get a Let's Encrypt
// certificate (obtained and renewed automatically, cached in
// <data>/acme); anything else (e.g. access by IP, or the local CLI) still
// gets the internal certificate. If Let's Encrypt fails, the internal
// certificate is served so the UI keeps working, and the error is logged.
//
// Let's Encrypt verifies the domain by connecting to it on port 443 (the
// TLS-ALPN challenge, answered here, so forward 443 to the UI port) or, if
// ACMEHTTPListen is set, on port 80 (HTTP challenge).
func (c *Controller) webTLS(internal tls.Certificate) (cfg *tls.Config, httpChallenge *http.Server) {
	cfg = &tls.Config{Certificates: []tls.Certificate{internal}, MinVersion: tls.VersionTLS12}
	domain := strings.ToLower(strings.TrimSpace(c.cfg.Domain))
	if domain == "" {
		return cfg, nil
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(c.cfg.path("acme")),
		HostPolicy: autocert.HostWhitelist(domain),
		Email:      c.cfg.ACMEEmail,
	}
	var logMu sync.Mutex
	var lastLogged time.Time
	cfg = &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1", acme.ALPNProto},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if !strings.EqualFold(hello.ServerName, domain) {
				return &internal, nil
			}
			cert, err := m.GetCertificate(hello)
			if err == nil {
				return cert, nil
			}
			// Challenge connections must fail rather than fall back.
			for _, p := range hello.SupportedProtos {
				if p == acme.ALPNProto {
					return nil, err
				}
			}
			logMu.Lock()
			if time.Since(lastLogged) > 10*time.Minute {
				lastLogged = time.Now()
				c.log.Warn("Let's Encrypt certificate unavailable; serving the internal certificate",
					"domain", domain, "err", err)
			}
			logMu.Unlock()
			return &internal, nil
		},
	}
	if c.cfg.ACMEHTTPListen != "" {
		httpChallenge = &http.Server{
			Addr:              c.cfg.ACMEHTTPListen,
			Handler:           m.HTTPHandler(nil), // answers challenges, redirects the rest to HTTPS
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	c.log.Info("web UI uses Let's Encrypt", "domain", domain, "http_challenge", c.cfg.ACMEHTTPListen)
	return cfg, httpChallenge
}

func shutdownQuietly(s *http.Server) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}
