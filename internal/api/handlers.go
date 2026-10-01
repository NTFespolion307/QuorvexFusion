package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/version"
)

// --- login / info ---

type loginRequest struct {
	Password  string `json:"password"`
	TokenName string `json:"token_name"`
}

type loginResponse struct {
	Token         string `json:"token"`
	CAFingerprint string `json:"ca_fingerprint"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.loginLimiter.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts; wait a minute")
		return
	}
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.c.CheckAdminPassword(req.Password) {
		s.log.Warn("failed login", "ip", ip)
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	name := strings.TrimSpace(req.TokenName)
	if name == "" {
		name = "login from " + ip
	}
	tok, _, err := s.c.CreateAPIToken(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("login", "ip", ip, "token_name", name)
	writeJSON(w, http.StatusOK, loginResponse{Token: tok, CAFingerprint: s.c.CAFingerprint()})
}

type infoResponse struct {
	Version       string `json:"version"`
	CAFingerprint string `json:"ca_fingerprint"`
	CACert        string `json:"ca_cert"`
	NodeAddr      string `json:"node_addr"`
	UIURL         string `json:"ui_url"`
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	cfg := s.c.Config()
	writeJSON(w, http.StatusOK, infoResponse{
		Version: version.Version, CAFingerprint: s.c.CAFingerprint(), CACert: string(s.c.CACertPEM()),
		NodeAddr: cfg.NodeAddr(), UIURL: cfg.UIURL(),
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	p, err := s.c.Pool()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// --- nodes ---

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.c.ListNodeViews()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

// nodeID resolves the {id} path value, which may also be a unique name.
func (s *Server) nodeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	n, err := s.c.FindNode(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return n.ID, true
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	v, err := s.c.NodeView(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) nodeHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.c.NodeHistory(id))
}

func (s *Server) approveNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	if err := s.c.ApproveNode(id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	if err := s.c.RevokeNode(id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	if err := s.c.DeleteNode(id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setNodeLabels(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeID(w, r)
	if !ok {
		return
	}
	var labels map[string]string
	if err := readJSON(r, &labels); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.c.SetNodeLabels(id, labels); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- join tokens ---

type createJoinTokenRequest struct {
	Description string `json:"description"`
	ExpiresIn   string `json:"expires_in"` // Go duration, e.g. "24h"; empty = never
	MaxUses     *int   `json:"max_uses"`
	AutoApprove bool   `json:"auto_approve"`
	Location    string `json:"location"`
	Ephemeral   bool   `json:"ephemeral"`
}

type createJoinTokenResponse struct {
	Token     string                  `json:"token"`
	JoinToken any                     `json:"join_token"`
	Commands  controller.JoinCommands `json:"commands"`
}

func (s *Server) listJoinTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.c.Store().ListJoinTokens()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toks)
}

func (s *Server) createJoinToken(w http.ResponseWriter, r *http.Request) {
	var req createJoinTokenRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts := controller.JoinTokenOptions{
		Description: req.Description, MaxUses: req.MaxUses, AutoApprove: req.AutoApprove,
		Location: strings.TrimSpace(req.Location), Ephemeral: req.Ephemeral,
	}
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "expires_in must be a positive duration like 24h")
			return
		}
		t := time.Now().Add(d)
		opts.ExpiresAt = &t
	}
	if req.MaxUses != nil && *req.MaxUses < 1 {
		writeError(w, http.StatusBadRequest, "max_uses must be at least 1")
		return
	}
	tok, rec, err := s.c.CreateJoinToken(opts)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, createJoinTokenResponse{Token: tok, JoinToken: rec, Commands: s.c.JoinCommands(tok)})
}

func (s *Server) revokeJoinToken(w http.ResponseWriter, r *http.Request) {
	if err := s.c.RevokeJoinToken(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- API tokens ---

func (s *Server) listAPITokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.c.Store().ListAPITokens()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toks)
}

func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	tok, rec, err := s.c.CreateAPIToken(strings.TrimSpace(req.Name))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "api_token": rec})
}

func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	if err := s.c.Store().DeleteAPIToken(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
