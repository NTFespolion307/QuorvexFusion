package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
)

// Browser sessions. The web UI logs in with the admin password and gets an
// HttpOnly, Secure, SameSite=Strict cookie; the API accepts that cookie as
// an alternative to a bearer token.
//
// Cross-site request forgery: SameSite=Strict already keeps the cookie off
// cross-site requests; additionally, any state-changing request
// authenticated by cookie must carry an Origin header naming this host.

const sessionCookie = "qf_session"

func sessionID(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// HasSession reports whether the request carries a valid UI session.
func (s *Server) HasSession(r *http.Request) bool { return s.c.SessionValid(sessionID(r)) }

// sameOrigin checks the Origin header of a cookie-authenticated request.
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// createSession: POST /api/v1/session {"password": "..."} logs a browser in.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.loginLimiter.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts; wait a minute")
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin login refused")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.c.CheckAdminPassword(req.Password) {
		s.log.Warn("failed UI login", "ip", ip)
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	id, err := s.c.CreateSession(ip)
	if err != nil {
		writeErr(w, err)
		return
	}
	setSessionCookie(w, id, int(controller.SessionTTL()/time.Second))
	s.log.Info("UI login", "ip", ip)
	w.WriteHeader(http.StatusNoContent)
}

// deleteSession: DELETE /api/v1/session logs out.
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if id := sessionID(r); id != "" {
		_ = s.c.DeleteSession(id)
	}
	setSessionCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// events: GET /api/v1/events is a Server-Sent Events stream of topic names
// ("nodes", "jobs", "metrics"). Each event means "this view changed,
// refetch it", which keeps the protocol trivial.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // disable buffering in nginx-style proxies
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, unsubscribe := s.c.Subscribe()
	defer unsubscribe()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case topic := <-ch:
			fmt.Fprintf(w, "event: %s\ndata: {}\n\n", topic)
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		}
		flusher.Flush()
	}
}

func (s *Server) poolHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.c.PoolHistory())
}

func (s *Server) drainNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	var req struct {
		Draining bool `json:"draining"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.c.SetNodeDraining(id, req.Draining); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.loginLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts; wait a minute")
		return
	}
	if err := s.c.ChangePassword(req.Current, req.New); err != nil {
		writeErr(w, err)
		return
	}
	setSessionCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
