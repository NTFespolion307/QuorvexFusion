package controller

import (
	"testing"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

func TestIsPublicAddr(t *testing.T) {
	local := []string{"192.168.0.120:5000", "10.1.2.3:1", "172.16.5.5:1", "127.0.0.1:9", "[::1]:9",
		"100.101.102.103:7443" /* Tailscale */, "[fd7a:115c:a1e0::1]:1" /* ULA */, "169.254.1.1:1", "[fe80::1]:1"}
	public := []string{"8.8.8.8:443", "203.0.113.7:51000", "[2001:4860:4860::8888]:443", "[::ffff:1.2.3.4]:5"}
	for _, a := range local {
		if isPublicAddr(a) {
			t.Errorf("%s classified public", a)
		}
	}
	for _, a := range public {
		if !isPublicAddr(a) {
			t.Errorf("%s classified local", a)
		}
	}
	n := &store.Node{Addr: "8.8.8.8:1", Network: "local"}
	if isRemote(n) {
		t.Error("admin override ignored")
	}
	n = &store.Node{Addr: "192.168.1.5:1", Network: "remote"}
	if !isRemote(n) {
		t.Error("admin override ignored")
	}
}

func TestEphemeralRemoval(t *testing.T) {
	c, _ := newTestController(t)
	c.cfg.EphemeralTimeoutSec = 600
	now := time.Now()
	old := now.Add(-time.Hour)
	recent := now.Add(-time.Minute)
	mk := func(id string, ephemeral bool, seen time.Time) {
		if err := c.store.CreateNode(&store.Node{ID: id, Name: id, Status: store.NodeApproved, PubKeyFP: id,
			Ephemeral: ephemeral, CreatedAt: old, ApprovedAt: &old}); err != nil {
			t.Fatal(err)
		}
		_ = c.store.TouchNode(id, seen)
	}
	mk("gone", true, old)      // ephemeral, offline for an hour: removed
	mk("blip", true, recent)   // ephemeral, offline a minute: kept
	mk("home", false, old)     // not ephemeral: kept forever
	mk("online", true, old)    // ephemeral but connected: kept
	addNode(t, c, "online", 4) // connect it

	// A task running on the vanished node is requeued, not lost.
	g := connectNode(c, "gone", 4, nil)
	j := submit(t, c, JobSpec{Command: "sleep 100", Requires: map[string]string{"hostname": "gone"}})
	pass(t, c)
	drain(g)
	disconnect(c, g)
	_ = c.store.TouchNode("gone", old)

	c.removeStaleEphemeral(now)
	for id, want := range map[string]bool{"gone": false, "blip": true, "home": true, "online": true} {
		_, err := c.store.GetNode(id)
		if (err == nil) != want {
			t.Errorf("node %s present=%v, want %v", id, err == nil, want)
		}
	}
	if tk := task(t, c, store.TaskID(j.ID, 0)); tk.State != store.TaskQueued || tk.Lost != 1 {
		t.Errorf("task on removed node: state=%s lost=%d, want requeued", tk.State, tk.Lost)
	}

	c.cfg.EphemeralTimeoutSec = 0 // disabled
	_ = c.store.TouchNode("blip", old)
	c.removeStaleEphemeral(now)
	if _, err := c.store.GetNode("blip"); err != nil {
		t.Error("removal ran although disabled")
	}
}

func TestRemoteNodesAvoidedWhenRequested(t *testing.T) {
	c, _ := newTestController(t)
	home := addNode(t, c, "home", 4)
	cloud := addNode(t, c, "cloud", 64)
	_ = c.store.SetNodeSettings("cloud", store.NodeSettings{Network: "remote"})
	no := false
	j := submit(t, c, JobSpec{Command: "x", AllowRemote: &no, CPUs: 8}) // too big for "home"
	pass(t, c)
	if as, _ := drain(cloud); len(as) != 0 {
		t.Fatal("job that refuses remote nodes ran on one")
	}
	if as, _ := drain(home); len(as) != 0 {
		t.Fatal("task placed on a node that is too small")
	}
	if r := c.PendingReason(store.TaskID(j.ID, 0)); r == "" {
		t.Error("no pending reason")
	}
	// Local nodes win when both fit.
	submit(t, c, JobSpec{Command: "y", CPUs: 1})
	pass(t, c)
	if as, _ := drain(home); len(as) != 1 {
		t.Error("small job not placed on the local node")
	}
}
