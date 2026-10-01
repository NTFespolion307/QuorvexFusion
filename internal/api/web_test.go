package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// browser returns a client that keeps cookies, like a browser would. The
// session cookie is Secure, so the test server must use TLS.
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:           jar,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newTLSServer(t *testing.T) *httptest.Server {
	plain := newTestServer(t) // reuse setup, then serve the same handler over TLS
	srv := httptest.NewTLSServer(plain.Config.Handler)
	t.Cleanup(srv.Close)
	return srv
}

func send(t *testing.T, c *http.Client, method, url, body, origin string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestOnlyLoginIsPublic(t *testing.T) {
	srv := newTLSServer(t)
	c := browser(t)
	if r := send(t, c, "GET", srv.URL+"/login", "", ""); r.StatusCode != 200 {
		t.Errorf("login page: %d", r.StatusCode)
	}
	if r := send(t, c, "GET", srv.URL+"/ui/public/login.js", "", ""); r.StatusCode != 200 {
		t.Errorf("login script: %d", r.StatusCode)
	}
	if r := send(t, c, "GET", srv.URL+"/", "", ""); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Errorf("index without session: %d -> %q", r.StatusCode, r.Header.Get("Location"))
	}
	for _, p := range []string{"/ui/js/app.js", "/ui/vendor/uPlot.iife.min.js", "/api/v1/nodes", "/api/v1/events"} {
		if r := send(t, c, "GET", srv.URL+p, "", ""); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without session: %d", p, r.StatusCode)
		}
	}
	if r := send(t, c, "GET", srv.URL+"/ui/public/", "", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("directory listing: %d", r.StatusCode)
	}
	if r := send(t, c, "GET", srv.URL+"/", "", ""); !strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Error("missing Content-Security-Policy")
	}
}

func TestSessionLoginAndCSRF(t *testing.T) {
	srv := newTLSServer(t)
	c := browser(t)
	if r := send(t, c, "POST", srv.URL+"/api/v1/session", `{"password":"wrong"}`, srv.URL); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if r := send(t, c, "POST", srv.URL+"/api/v1/session", `{"password":"admin-password"}`, "https://evil.example"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", r.StatusCode)
	}
	r := send(t, c, "POST", srv.URL+"/api/v1/session", `{"password":"admin-password"}`, srv.URL)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("login: %d", r.StatusCode)
	}
	ck := r.Cookies()
	if len(ck) != 1 || !ck[0].HttpOnly || !ck[0].Secure || ck[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie attributes: %+v", ck)
	}

	if r := send(t, c, "GET", srv.URL+"/", "", ""); r.StatusCode != 200 {
		t.Errorf("index with session: %d", r.StatusCode)
	}
	if r := send(t, c, "GET", srv.URL+"/api/v1/nodes", "", ""); r.StatusCode != 200 {
		t.Errorf("API with session: %d", r.StatusCode)
	}
	// A state-changing request with the cookie but from another origin
	// (or without an Origin header) is refused.
	body := `{"description":"x"}`
	if r := send(t, c, "POST", srv.URL+"/api/v1/join-tokens", body, "https://evil.example"); r.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d", r.StatusCode)
	}
	if r := send(t, c, "POST", srv.URL+"/api/v1/join-tokens", body, ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("POST without Origin: %d", r.StatusCode)
	}
	if r := send(t, c, "POST", srv.URL+"/api/v1/join-tokens", body, srv.URL); r.StatusCode != http.StatusCreated {
		t.Errorf("same-origin POST: %d", r.StatusCode)
	}

	// Logout ends the session.
	if r := send(t, c, "DELETE", srv.URL+"/api/v1/session", "", srv.URL); r.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if r := send(t, c, "GET", srv.URL+"/api/v1/nodes", "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("API after logout: %d", r.StatusCode)
	}
}

func TestEventStream(t *testing.T) {
	srv := newTLSServer(t)
	c := browser(t)
	send(t, c, "POST", srv.URL+"/api/v1/session", `{"password":"admin-password"}`, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	lines := bufio.NewReader(resp.Body)
	if l, _ := lines.ReadString('\n'); !strings.HasPrefix(l, ": connected") {
		t.Fatalf("first line %q", l)
	}

	// Creating a job publishes a "jobs" event.
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/jobs", strings.NewReader(`{"command":"echo hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", srv.URL)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	for {
		l, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		if strings.TrimSpace(l) == "event: jobs" {
			break
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(lines, 0))
}
