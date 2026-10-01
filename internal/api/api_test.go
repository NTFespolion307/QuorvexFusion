package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := controller.DefaultConfig(filepath.Join(t.TempDir(), "ctl"))
	if _, err := controller.Init(cfg, "admin-password"); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := controller.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Store().Close() })
	srv := httptest.NewServer(New(c, log).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp, out
}

func TestAuthRequired(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/api/v1/nodes", "/api/v1/status", "/api/v1/join-tokens", "/api/v1/info"} {
		if resp, _ := do(t, srv, "GET", path, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", path, resp.StatusCode)
		}
		if resp, _ := do(t, srv, "GET", path, "cat_abcd_bogus", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with bogus token: %d", path, resp.StatusCode)
		}
	}
}

func TestLoginAndJoinToken(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := do(t, srv, "POST", "/api/v1/login", "", map[string]string{"password": "nope"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, out := do(t, srv, "POST", "/api/v1/login", "", map[string]string{"password": "admin-password", "token_name": "test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %v", resp.StatusCode, out)
	}
	token, _ := out["token"].(string)
	if !strings.HasPrefix(token, "cat_") {
		t.Fatalf("token = %q", token)
	}

	resp, out = do(t, srv, "POST", "/api/v1/join-tokens", token, map[string]any{"expires_in": "1h", "max_uses": 3, "location": "vastai", "ephemeral": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create join token: %d %v", resp.StatusCode, out)
	}
	cmds, _ := out["commands"].(map[string]any)
	if s, _ := cmds["bootstrap"].(string); !strings.Contains(s, "--ca-fingerprint sha256:") {
		t.Errorf("bootstrap command lacks fingerprint: %q", s)
	}

	resp, _ = do(t, srv, "POST", "/api/v1/join-tokens", token, map[string]any{"expires_in": "soon"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad duration accepted: %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	srv := newTestServer(t)
	limited := false
	for i := 0; i < 10; i++ {
		resp, _ := do(t, srv, "POST", "/api/v1/login", "", map[string]string{"password": "guess"})
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("10 rapid failed logins were never rate limited")
	}
}
