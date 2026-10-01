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

	// DisableMDNS stops advertising the controller on the local network.
	DisableMDNS bool `json:"disable_mdns,omitempty"`
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
	if ip := outboundIPv4(); ip != "" {
		return ip
	}
	if ips := localIPv4s(); len(ips) > 0 {
		return ips[0]
	}
	return "localhost"
}

// outboundIPv4 is the address of the interface with the default route,
// usually the one other machines can reach. "Dialing" UDP sends nothing; it
// just makes the kernel pick a source address.
func outboundIPv4() string {
	conn, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, never routed anywhere
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsLoopback() {
		return a.IP.String()
	}
	return ""
}

// advertiseIPs are the addresses announced over mDNS.
func (c *Config) advertiseIPs() []net.IP {
	if host, _, err := net.SplitHostPort(c.NodeListen); err == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
			return []net.IP{ip}
		}
	}
	// The default-route address first: mDNS clients typically use the first.
	var out []net.IP
	first := outboundIPv4()
	if first != "" {
		out = append(out, net.ParseIP(first))
	}
	for _, s := range localIPv4s() {
		if s != first {
			out = append(out, net.ParseIP(s))
		}
	}
	return out
}

// localIPv4s lists IPv4 addresses on interfaces that are up and not
// loopback interfaces (checked by flag: some systems, e.g. WSL, put
// non-127.x addresses on "lo").
func localIPv4s() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}
