package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/NTFespolion307/QuorvexFusion/internal/client"
	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

func apiClient() (*client.Client, error) {
	cfg, err := client.LoadConfig(globalFlags.config)
	if err != nil {
		return nil, err
	}
	return client.New(cfg)
}

// call is a shorthand for one API request from a CLI command.
func call(method, path string, in, out any) error {
	c, err := apiClient()
	if err != nil {
		return err
	}
	return c.Do(context.Background(), method, path, in, out)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTable() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0) }

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// --- login ---

func loginCmd() *cobra.Command {
	var controllerURL, fingerprint, tokenName string
	var passwordStdin, yes bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to a controller with the admin password and save an API token",
		Example: `  cluster login --controller 192.168.1.10:8443
  echo "$PW" | cluster login --controller ctl.example.com:8443 --password-stdin --ca-fingerprint sha256:...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if controllerURL == "" {
				return errors.New("--controller is required (web UI address, e.g. host:8443)")
			}
			base := client.NormalizeURL(controllerURL)
			u, err := url.Parse(base)
			if err != nil {
				return err
			}
			hostport := u.Host
			if u.Port() == "" {
				hostport += ":443"
			}

			cfg := &client.Config{Controller: base}
			// A certificate from a public CA (e.g. Let's Encrypt) needs no
			// pinning. Otherwise pin the controller's own CA, after the
			// user checks its fingerprint.
			if !publiclyTrusted(base) {
				ca, err := pki.FetchCA(hostport, 15*time.Second)
				if err != nil {
					return fmt.Errorf("contact %s: %w", hostport, err)
				}
				fp := pki.Fingerprint(ca)
				switch {
				case fingerprint != "":
					if fp != pki.NormalizeFingerprint(fingerprint) {
						return fmt.Errorf("CA fingerprint mismatch: got %s", fp)
					}
				case yes:
					fmt.Fprintf(os.Stderr, "WARNING: trusting controller CA %s without verification\n", fp)
				default:
					ok := fingerprintConfirmer(false)
					if ok == nil {
						return errors.New("cannot confirm the controller fingerprint without a terminal; pass --ca-fingerprint or --yes")
					}
					if !ok(fp) {
						return errors.New("fingerprint not confirmed")
					}
				}
				cfg.CACert = string(pki.CertPEM(ca))
			}

			var pw string
			if passwordStdin {
				pw, err = readStdinLine()
			} else {
				pw, err = readPassword("Admin password: ", false)
			}
			if err != nil {
				return err
			}
			if tokenName == "" {
				h, _ := os.Hostname()
				tokenName = "cli@" + h
			}
			c, err := client.New(cfg)
			if err != nil {
				return err
			}
			var resp struct {
				Token string `json:"token"`
			}
			if err := c.Do(context.Background(), "POST", "/api/v1/login",
				map[string]string{"password": pw, "token_name": tokenName}, &resp); err != nil {
				return err
			}
			cfg.Token = resp.Token
			path := globalFlags.config
			if path == "" {
				path = client.DefaultConfigPath()
			}
			if err := client.SaveConfig(path, cfg); err != nil {
				return err
			}
			fmt.Printf("Logged in to %s. Config saved to %s\n", base, path)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&controllerURL, "controller", "", "controller web/API address, e.g. host:8443")
	f.StringVar(&fingerprint, "ca-fingerprint", "", "expected controller CA fingerprint")
	f.StringVar(&tokenName, "token-name", "", "name for the created API token")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	f.BoolVar(&yes, "yes", false, "trust the controller CA without confirmation")
	return cmd
}

// publiclyTrusted reports whether base presents a certificate valid under
// the system's root CAs (so no pinning is needed).
func publiclyTrusted(base string) bool {
	c := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	}
	resp, err := c.Get(base + "/api/v1/info")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// --- status ---

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the resource pool and task counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			var p controller.PoolSummary
			if err := call("GET", "/api/v1/status", nil, &p); err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(p)
			}
			fmt.Printf("Nodes: %d online, %d offline, %d pending\n", p.NodesOnline, p.NodesOffline, p.NodesPending)
			fmt.Printf("Tasks: %d running, %d queued\n\n", p.Tasks.Running+p.Tasks.Assigned, p.Tasks.Queued)
			t := newTable()
			fmt.Fprintln(t, "LOCATION\tCPUS (used/total)\tMEMORY (used/total)\tGPUS (used/total)")
			row := func(name string, used, total controller.Resources) {
				fmt.Fprintf(t, "%s\t%.4g / %.4g\t%s / %s\t%d / %d\n", name, used.CPUs, total.CPUs,
					humanBytes(used.MemoryBytes), humanBytes(total.MemoryBytes), used.GPUs, total.GPUs)
			}
			for _, loc := range p.Locations {
				l := p.ByLocation[loc]
				row(loc, l.Used, l.Total)
			}
			row("TOTAL", p.Used, p.Total)
			return t.Flush()
		},
	}
}

// --- nodes ---

func nodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "nodes",
		Aliases: []string{"node"},
		Short:   "List and manage nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			var nodes []*controller.NodeView
			if err := call("GET", "/api/v1/nodes", nil, &nodes); err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(nodes)
			}
			t := newTable()
			fmt.Fprintln(t, "ID\tNAME\tSTATUS\tLOCATION\tCPUS\tMEMORY\tGPUS\tCPU%\tMEM%\tRTT\tADDRESS")
			for _, n := range nodes {
				cpus, mem, gpus, cpuPct, memPct, rtt := "-", "-", "-", "-", "-", "-"
				if hw := n.Hardware; hw != nil {
					cpus = fmt.Sprintf("%.4g/%.4g", n.Used.CPUs, hw.CpuLimit)
					mem = humanBytes(n.Used.MemoryBytes) + "/" + humanBytes(hw.MemoryBytes)
					gpus = fmt.Sprintf("%d/%d", n.Used.GPUs, len(hw.Gpus))
				}
				if m := n.Metrics; m != nil {
					cpuPct = fmt.Sprintf("%.0f", m.CpuPercent)
					if m.MemTotalBytes > 0 {
						memPct = fmt.Sprintf("%.0f", 100*float64(m.MemUsedBytes)/float64(m.MemTotalBytes))
					}
				}
				if n.Status == "online" {
					rtt = fmt.Sprintf("%.1fms", n.RTTMillis)
				}
				status := n.Status
				if n.Ephemeral {
					status += ",ephemeral"
				}
				if n.Remote {
					status += ",remote"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					n.ID, n.Name, status, n.Location, cpus, mem, gpus, cpuPct, memPct, rtt, n.Addr)
			}
			return t.Flush()
		},
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "show <node>",
		Short: "Show a node's hardware and live metrics",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var n controller.NodeView
			if err := call("GET", "/api/v1/nodes/"+url.PathEscape(args[0]), nil, &n); err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(n)
			}
			printNode(&n)
			return nil
		},
	})

	var location, network string
	var ephemeral bool
	var labels []string
	setCmd := &cobra.Command{
		Use:   "set <node>",
		Short: "Change a node's location, ephemeral flag, local/remote setting or labels",
		Example: `  cluster nodes set gpu-box-1 --location vastai --ephemeral
  cluster nodes set office-pc --network local --label gpu=3090
  cluster nodes set office-pc --network auto`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var n controller.NodeView
			path := "/api/v1/nodes/" + url.PathEscape(args[0])
			if err := call("GET", path, nil, &n); err != nil {
				return err
			}
			f := cmd.Flags()
			ns := store.NodeSettings{Location: n.Location, Ephemeral: n.Ephemeral, Network: n.Network}
			if f.Changed("location") {
				ns.Location = location
			}
			if f.Changed("ephemeral") {
				ns.Ephemeral = ephemeral
			}
			if f.Changed("network") {
				ns.Network = strings.TrimPrefix(network, "auto")
			}
			if err := call("PUT", path+"/settings", ns, nil); err != nil {
				return err
			}
			if f.Changed("label") {
				l, err := parseKV(labels, "--label")
				if err != nil {
					return err
				}
				if err := call("PUT", path+"/labels", l, nil); err != nil {
					return err
				}
			}
			fmt.Printf("%s: updated\n", n.Name)
			return nil
		},
	}
	sf := setCmd.Flags()
	sf.StringVar(&location, "location", "", "location label, e.g. home, vastai")
	sf.BoolVar(&ephemeral, "ephemeral", false, "rented/cloud node, removed automatically when offline too long")
	sf.StringVar(&network, "network", "", "auto (detect from its address), local or remote")
	sf.StringArrayVar(&labels, "label", nil, "replace the admin labels with these KEY=VALUE labels (repeatable)")
	cmd.AddCommand(setCmd)

	for _, action := range []struct{ name, short, method, suffix string }{
		{"approve", "Approve a pending node", "POST", "/approve"},
		{"revoke", "Revoke a node and disconnect it immediately", "POST", "/revoke"},
		{"rm", "Forget a node (revoke it first if online)", "DELETE", ""},
	} {
		cmd.AddCommand(&cobra.Command{
			Use:   action.name + " <node>...",
			Short: action.short,
			Args:  cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				for _, ref := range args {
					if err := call(action.method, "/api/v1/nodes/"+url.PathEscape(ref)+action.suffix, nil, nil); err != nil {
						return fmt.Errorf("%s: %w", ref, err)
					}
					fmt.Printf("%s: done\n", ref)
				}
				return nil
			},
		})
	}
	return cmd
}

func printNode(n *controller.NodeView) {
	t := newTable()
	row := func(k, v string) { fmt.Fprintf(t, "%s:\t%s\n", k, v) }
	row("ID", n.ID)
	row("Name", n.Name)
	row("Status", n.Status)
	row("Location", n.Location)
	row("Ephemeral", fmt.Sprint(n.Ephemeral))
	network := "local"
	if n.Remote {
		network = "remote"
	}
	if n.Network == "" {
		network += " (detected from its address)"
	} else {
		network += " (set by admin)"
	}
	row("Connection", network)
	row("Address", n.Addr)
	if n.Status == "online" {
		row("Latency", fmt.Sprintf("%.1f ms", n.RTTMillis))
	}
	row("Last seen", ago(n.LastSeen))
	if len(n.Labels) > 0 {
		var ls []string
		for k, v := range n.Labels {
			ls = append(ls, k+"="+v)
		}
		sort.Strings(ls)
		row("Labels", strings.Join(ls, " "))
	}
	if hw := n.Hardware; hw != nil {
		row("Hostname", hw.Hostname)
		row("OS", fmt.Sprintf("%s (kernel %s, %s)", hw.Os, hw.Kernel, hw.Arch))
		row("CPU", fmt.Sprintf("%s, %d cores / %d threads, usable %.4g", hw.CpuModel, hw.PhysicalCores, hw.LogicalCores, hw.CpuLimit))
		row("Memory", humanBytes(hw.MemoryBytes))
		for _, d := range hw.Disks {
			row("Disk "+d.Mount, fmt.Sprintf("%s free of %s (%s)", humanBytes(d.FreeBytes), humanBytes(d.TotalBytes), d.Fstype))
		}
		for _, g := range hw.Gpus {
			row(fmt.Sprintf("GPU %d", g.Index), fmt.Sprintf("%s %s, %s", g.Vendor, g.Name, humanBytes(g.MemoryBytes)))
		}
		row("Container", fmt.Sprint(hw.InContainer))
		row("Docker", fmt.Sprintf("%v (nvidia: %v)", hw.Docker, hw.NvidiaDocker))
		row("systemd", fmt.Sprint(hw.Systemd))
		row("IPs", strings.Join(hw.Ips, " "))
	}
	if m := n.Metrics; m != nil {
		row("CPU usage", fmt.Sprintf("%.1f%% (load %.2f %.2f %.2f)", m.CpuPercent, m.Load1, m.Load5, m.Load15))
		row("Memory used", fmt.Sprintf("%s of %s", humanBytes(m.MemUsedBytes), humanBytes(m.MemTotalBytes)))
		row("Network", fmt.Sprintf("rx %s/s, tx %s/s", humanBytes(m.NetRxBytesPerSec), humanBytes(m.NetTxBytesPerSec)))
		for _, g := range m.Gpus {
			row(fmt.Sprintf("GPU %d usage", g.Index), fmt.Sprintf("%.0f%%, %s/%s, %.0f°C, %.0fW",
				g.UtilizationPercent, humanBytes(g.MemoryUsedBytes), humanBytes(g.MemoryTotalBytes), g.TemperatureC, g.PowerWatts))
		}
		row("Uptime", (time.Duration(m.UptimeSeconds) * time.Second).String())
	}
	t.Flush()
}

// --- join tokens ---

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage join tokens (used by new workers to join)",
	}

	var description, expires, location string
	var maxUses int
	var manual, ephemeral bool
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a join token and print the commands to join a worker",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := map[string]any{
				"description": description, "expires_in": expires, "auto_approve": !manual,
				"location": location, "ephemeral": ephemeral,
			}
			if maxUses > 0 {
				req["max_uses"] = maxUses
			}
			var resp struct {
				Token         string                  `json:"token"`
				Code          string                  `json:"code"`
				CAFingerprint string                  `json:"ca_fingerprint"`
				NodeAddr      string                  `json:"node_addr"`
				JoinToken     store.JoinToken         `json:"join_token"`
				Commands      controller.JoinCommands `json:"commands"`
			}
			if err := call("POST", "/api/v1/join-tokens", req, &resp); err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(resp)
			}
			fmt.Printf("Join code (shown only once):  %s\n\n", resp.Code)
			fmt.Printf("On the new machine, in a checkout of the repository, run\n  sudo ./install.sh worker\n")
			fmt.Printf("and enter the code; on another network add --controller %s --code %s\n\n", resp.NodeAddr, resp.Code)
			fmt.Printf("One line, for cloud-init or a vast.ai on-start script (run as root):\n  %s\n\n", resp.Commands.Bootstrap)
			fmt.Printf("With the binary already installed:\n  %s\n\n", resp.Commands.Direct)
			fmt.Printf("For scripts, the same token in long form (use with --ca-fingerprint %s):\n  %s\n", resp.CAFingerprint, resp.Token)
			return nil
		},
	}
	f := create.Flags()
	f.StringVar(&description, "description", "", "note shown in the token list")
	f.StringVar(&expires, "expires", "", "lifetime, e.g. 24h or 168h (default: never)")
	f.IntVar(&maxUses, "max-uses", 0, "maximum number of joins (default: unlimited)")
	f.BoolVar(&manual, "manual-approve", false, "new nodes stay pending until approved")
	f.StringVar(&location, "location", "", "default location for nodes joining with this token")
	f.BoolVar(&ephemeral, "ephemeral", false, "nodes joining with this token are ephemeral")

	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List join tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			var toks []*store.JoinToken
			if err := call("GET", "/api/v1/join-tokens", nil, &toks); err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(toks)
			}
			t := newTable()
			fmt.Fprintln(t, "ID\tSTATE\tUSES\tAPPROVAL\tLOCATION\tEXPIRES\tDESCRIPTION")
			for _, tk := range toks {
				state := "active"
				if err := tk.Usable(time.Now()); err != nil {
					state = "unusable"
					if tk.RevokedAt != nil {
						state = "revoked"
					}
				}
				uses := fmt.Sprint(tk.Uses)
				if tk.MaxUses != nil {
					uses += fmt.Sprintf("/%d", *tk.MaxUses)
				}
				approval := "auto"
				if !tk.AutoApprove {
					approval = "manual"
				}
				exp := "never"
				if tk.ExpiresAt != nil {
					exp = tk.ExpiresAt.Local().Format("2006-01-02 15:04")
				}
				loc := tk.Location
				if tk.Ephemeral {
					loc += " (ephemeral)"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", tk.ID, state, uses, approval, loc, exp, tk.Description)
			}
			return t.Flush()
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke <id>...",
		Short: "Revoke join tokens (nodes that already joined are unaffected)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, id := range args {
				if err := call("DELETE", "/api/v1/join-tokens/"+url.PathEscape(id), nil, nil); err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
				fmt.Printf("%s: revoked\n", id)
			}
			return nil
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

// --- API tokens ---

func apiTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apitoken",
		Short: "Manage API tokens (used by the CLI and scripts)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "create <name>",
			Short: "Create an API token",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				var resp struct {
					Token string `json:"token"`
				}
				if err := call("POST", "/api/v1/api-tokens", map[string]string{"name": args[0]}, &resp); err != nil {
					return err
				}
				fmt.Println(resp.Token)
				return nil
			},
		},
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List API tokens",
			RunE: func(cmd *cobra.Command, args []string) error {
				var toks []*store.APIToken
				if err := call("GET", "/api/v1/api-tokens", nil, &toks); err != nil {
					return err
				}
				if globalFlags.json {
					return printJSON(toks)
				}
				t := newTable()
				fmt.Fprintln(t, "ID\tNAME\tCREATED\tLAST USED")
				for _, tk := range toks {
					fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", tk.ID, tk.Name, tk.CreatedAt.Local().Format("2006-01-02 15:04"), ago(tk.LastUsed))
				}
				return t.Flush()
			},
		},
		&cobra.Command{
			Use:   "revoke <id>",
			Short: "Delete an API token",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return call("DELETE", "/api/v1/api-tokens/"+url.PathEscape(args[0]), nil, nil)
			},
		},
	)
	return cmd
}
