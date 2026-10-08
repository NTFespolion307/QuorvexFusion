package controller

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// Remote and ephemeral nodes.
//
// A node is "remote" when it connects from a public internet address, as
// opposed to the LAN or a VPN. The scheduler prefers local nodes, and jobs
// can refuse remote ones (--no-remote). An admin can override the
// detection per node (Network = "local" or "remote").
//
// "Ephemeral" nodes (rented/cloud machines) that stay offline longer than
// EphemeralTimeoutSec are removed automatically; their tasks were already
// requeued when they went offline.

// cgnat is 100.64.0.0/10: carrier-grade NAT, also used by Tailscale.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isPublicAddr reports whether a "host:port" connection address is on the
// public internet.
func isPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsUnspecified(), cgnat.Contains(ip):
		return false
	}
	return true
}

// isRemote applies the admin override, else the address check.
func isRemote(n *store.Node) bool {
	switch n.Network {
	case "local":
		return false
	case "remote":
		return true
	}
	return isPublicAddr(n.Addr)
}

// SetNodeSettings changes a node's location, ephemeral flag and network
// override.
func (c *Controller) SetNodeSettings(id string, ns store.NodeSettings) error {
	switch ns.Network {
	case "", "local", "remote":
	default:
		return errors.New(`network must be "", "local" or "remote"`)
	}
	if err := c.store.SetNodeSettings(id, ns); err != nil {
		return err
	}
	c.events.publish("nodes")
	c.kickScheduler()
	return nil
}

// removeStaleEphemeral deletes ephemeral nodes that have been offline for
// longer than the configured timeout.
func (c *Controller) removeStaleEphemeral(now time.Time) {
	timeout := time.Duration(c.cfg.EphemeralTimeoutSec) * time.Second
	if timeout <= 0 {
		return
	}
	nodes, err := c.store.ListNodes()
	if err != nil {
		return
	}
	for _, n := range nodes {
		if !n.Ephemeral || n.Status != store.NodeApproved || c.hub.Get(n.ID) != nil {
			continue
		}
		seen := n.CreatedAt
		if n.ApprovedAt != nil {
			seen = *n.ApprovedAt
		}
		if n.LastSeen != nil && n.LastSeen.After(seen) {
			seen = *n.LastSeen
		}
		if now.Sub(seen) < timeout {
			continue
		}
		// Requeue anything still attributed to it (normally already done
		// when it went offline), then forget it.
		c.tm.mu.Lock()
		c.loseNodeAttemptsLocked(n.ID, "ephemeral node removed")
		delete(c.tm.cached, n.ID)
		c.tm.mu.Unlock()
		if err := c.store.DeleteNode(n.ID); err != nil {
			c.log.Error("remove ephemeral node", "node", n.ID, "err", err)
			continue
		}
		c.log.Info("removed ephemeral node that stayed offline", "node", n.ID, "name", n.Name,
			"offline_for", now.Sub(seen).Round(time.Second))
		c.events.publish("nodes")
	}
}

func (c *Controller) ephemeralLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.removeStaleEphemeral(now)
		}
	}
}
