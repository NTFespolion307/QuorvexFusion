package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Config is stored as controller.json in the data directory, written by
// `cluster controller init` and editable by hand.
type Config struct {
	DataDir string `json:"-"`

	// NodeListen is where workers connect (gRPC, mutual TLS).
	NodeListen string `json:"node_listen"`
	// HTTPListen serves the web UI and REST API.
	HTTPListen string `json:"http_listen"`
	// PublicAddr is the host name or IP that workers and browsers use to
	// reach the controller (a domain, public IP, or Tailscale name). It is
	// added to the server certificate and used in printed join commands.
	PublicAddr string `json:"public_addr,omitempty"`
	// ExtraNames are added to the server certificate's names.
	ExtraNames []string `json:"extra_names,omitempty"`

	// HeartbeatTimeoutSec: a node silent this long is marked offline.
	HeartbeatTimeoutSec int `json:"heartbeat_timeout_sec"`
	// MetricsIntervalSec: how often workers send metrics.
	MetricsIntervalSec int `json:"metrics_interval_sec"`
}

func DefaultConfig(dataDir string) *Config {
	return &Config{
		DataDir:             dataDir,
		NodeListen:          ":7443",
		HTTPListen:          ":8443",
		HeartbeatTimeoutSec: 15,
		MetricsIntervalSec:  5,
	}
}

func (c *Config) path(name string) string { return filepath.Join(c.DataDir, name) }

func (c *Config) HeartbeatTimeout() time.Duration {
	return time.Duration(c.HeartbeatTimeoutSec) * time.Second
}

const configFile = "controller.json"

// LoadConfig reads controller.json from dataDir.
func LoadConfig(dataDir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s is not initialised; run `cluster controller init --data-dir %s` first", dataDir, dataDir)
	}
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig(dataDir)
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", configFile, err)
	}
	if cfg.HeartbeatTimeoutSec < 3 {
		cfg.HeartbeatTimeoutSec = 3
	}
	if cfg.MetricsIntervalSec < 1 {
		cfg.MetricsIntervalSec = 1
	}
	return cfg, nil
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path(configFile), append(data, '\n'), 0o600)
}

// NodeAddr is the host:port workers should dial, for printed join commands.
func (c *Config) NodeAddr() string {
	_, port, err := net.SplitHostPort(c.NodeListen)
	if err != nil {
		port = "7443"
	}
	return net.JoinHostPort(c.advertisedHost(), port)
}

// UIURL is the web UI address, for printed messages.
func (c *Config) UIURL() string {
	_, port, err := net.SplitHostPort(c.HTTPListen)
	if err != nil {
		port = "8443"
	}
	return "https://" + net.JoinHostPort(c.advertisedHost(), port)
}

func (c *Config) advertisedHost() string {
	if c.PublicAddr != "" {
		return c.PublicAddr
	}
	// If listening on a specific IP (e.g. a VPN interface), use it.
	if host, _, err := net.SplitHostPort(c.NodeListen); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		return host
	}
	if ips := localIPv4s(); len(ips) > 0 {
		return ips[0]
	}
	return "localhost"
}

func localIPv4s() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
