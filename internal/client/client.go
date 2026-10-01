// Package client is the CLI's HTTP client for the controller's REST API,
// plus the CLI config file that stores the controller address and token.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
)

// Config is stored as JSON, by default in ~/.config/cluster/cli.json.
type Config struct {
	Controller string `json:"controller"`        // e.g. https://10.0.0.5:8443
	Token      string `json:"token"`             // API token (cat_...)
	CACert     string `json:"ca_cert,omitempty"` // PEM; empty = use system roots (e.g. Let's Encrypt)
}

// SystemConfigPath is a machine-wide fallback written by install.sh on the
// controller, readable by root.
const SystemConfigPath = "/etc/cluster/cli.json"

// DefaultConfigPath is the per-user config location.
func DefaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "cluster", "cli.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "cli.json"
	}
	return filepath.Join(home, ".config", "cluster", "cli.json")
}

// LoadConfig reads the config from path (or the default locations when
// path is empty) and applies the CLUSTER_CONTROLLER, CLUSTER_TOKEN and
// CLUSTER_CA_CERT (a PEM file path) environment overrides.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	paths := []string{path}
	if path == "" {
		paths = []string{DefaultConfigPath(), SystemConfigPath}
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		break
	}
	if v := os.Getenv("CLUSTER_CONTROLLER"); v != "" {
		cfg.Controller = v
	}
	if v := os.Getenv("CLUSTER_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("CLUSTER_CA_CERT"); v != "" {
		pem, err := os.ReadFile(v)
		if err != nil {
			return nil, err
		}
		cfg.CACert = string(pem)
	}
	cfg.Controller = NormalizeURL(cfg.Controller)
	return cfg, nil
}

// NormalizeURL accepts "host:port" or a full URL.
func NormalizeURL(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

func SaveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

type Client struct {
	cfg  *Config
	http *http.Client
}

// New builds a client. If the config holds a CA certificate, the controller
// must present a certificate signed by it (host names are not checked, as
// for workers); otherwise normal public-CA verification applies.
func New(cfg *Config) (*Client, error) {
	if cfg.Controller == "" {
		return nil, errors.New("no controller configured: run `cluster login --controller host:8443` " +
			"or set CLUSTER_CONTROLLER and CLUSTER_TOKEN")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACert != "" {
		ca, err := pki.ParseCertPEM([]byte(cfg.CACert))
		if err != nil {
			return nil, fmt.Errorf("config ca_cert: %w", err)
		}
		tlsCfg = pki.PinnedClientTLS(ca, nil)
	}
	return &Client{cfg: cfg, http: &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true, Proxy: http.ProxyFromEnvironment},
	}}, nil
}

// APIError is a non-2xx response from the controller.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("controller: %s (HTTP %d)", e.Message, e.Status)
}

// Raw performs a request and returns the raw body and headers (used for
// log streaming).
func (c *Client) Raw(ctx context.Context, method, path string) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Controller+path, nil)
	if err != nil {
		return nil, nil, err
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, nil, &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	return body, resp.Header, nil
}

// Do sends a JSON request and decodes a JSON response into out (if non-nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Controller+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
