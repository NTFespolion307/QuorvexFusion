// Package api is the controller's REST API under /api/v1, used by both the
// CLI and the web UI.
//
// Every endpoint requires an API token (Authorization: Bearer cat_...),
// except POST /api/v1/login, which exchanges the admin password for a new
// API token and is rate-limited per client IP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/ratelimit"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
	"github.com/NTFespolion307/QuorvexFusion/internal/web"
)

type Server struct {
	c            *controller.Controller
	log          *slog.Logger
	loginLimiter *ratelimit.Limiter
}

func New(c *controller.Controller, log *slog.Logger) *Server {
	return &Server{
		c:   c,
		log: log,
		// 5 attempts at once per IP, then one every 12 seconds.
		loginLimiter: ratelimit.New(12*time.Second, 5),
	}
}

// Handler returns the HTTP handler for the API and the web UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The only endpoints usable without credentials: logging in (both
	// rate-limited) and, in package web, the login page itself.
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/session", s.createSession)
	web.Register(mux, s.HasSession)

	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/v1/info", s.info)
	authed.HandleFunc("GET /api/v1/status", s.status)
	authed.HandleFunc("GET /api/v1/events", s.events)
	authed.HandleFunc("GET /api/v1/pool/history", s.poolHistory)
	authed.HandleFunc("DELETE /api/v1/session", s.deleteSession)
	authed.HandleFunc("POST /api/v1/password", s.changePassword)

	authed.HandleFunc("GET /api/v1/nodes", s.listNodes)
	authed.HandleFunc("GET /api/v1/nodes/{id}", s.getNode)
	authed.HandleFunc("GET /api/v1/nodes/{id}/history", s.nodeHistory)
	authed.HandleFunc("POST /api/v1/nodes/{id}/approve", s.approveNode)
	authed.HandleFunc("POST /api/v1/nodes/{id}/revoke", s.revokeNode)
	authed.HandleFunc("PUT /api/v1/nodes/{id}/labels", s.setNodeLabels)
	authed.HandleFunc("POST /api/v1/nodes/{id}/drain", s.drainNode)
	authed.HandleFunc("DELETE /api/v1/nodes/{id}", s.deleteNode)

	authed.HandleFunc("POST /api/v1/jobs", s.submitJob)
	authed.HandleFunc("GET /api/v1/jobs", s.listJobs)
	authed.HandleFunc("GET /api/v1/jobs/{id}", s.getJob)
	authed.HandleFunc("GET /api/v1/jobs/{id}/tasks", s.listJobTasks)
	authed.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.cancelJob)
	authed.HandleFunc("DELETE /api/v1/jobs/{id}", s.deleteJob)
	authed.HandleFunc("GET /api/v1/tasks/{id}", s.getTask)
	authed.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.cancelTask)
	authed.HandleFunc("GET /api/v1/tasks/{id}/logs", s.taskLogs)

	authed.HandleFunc("GET /api/v1/join-tokens", s.listJoinTokens)
	authed.HandleFunc("POST /api/v1/join-tokens", s.createJoinToken)
	authed.HandleFunc("DELETE /api/v1/join-tokens/{id}", s.revokeJoinToken)

	authed.HandleFunc("GET /api/v1/api-tokens", s.listAPITokens)
	authed.HandleFunc("POST /api/v1/api-tokens", s.createAPIToken)
	authed.HandleFunc("DELETE /api/v1/api-tokens/{id}", s.deleteAPIToken)

	mux.Handle("/api/", s.requireAuth(authed))
	return securityHeaders(mux)
}

// --- middleware ---

type tokenKey struct{}

// requireAuth accepts an API token (CLI, scripts) or a UI session cookie.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			if s.HasSession(r) {
				if !sameOrigin(r) {
					writeError(w, http.StatusForbidden, "cross-origin request refused")
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, http.StatusUnauthorized, "not logged in (missing API token or session)")
			return
		}
		t, err := s.c.Store().CheckAPIToken(tok, time.Now())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid API token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, t)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		// The UI loads nothing from other origins. Inline styles are allowed
		// because the charting library sets element styles.
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// clientIP uses the TCP peer address. X-Forwarded-For is deliberately not
// trusted, so rate limits can't be bypassed by spoofing headers.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeErr maps common errors to HTTP status codes.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, controller.ErrAmbiguous):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}
