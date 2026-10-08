package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
)

// controllerSetCmd changes controller.json and restarts the service, so
// nobody has to edit JSON by hand.
func controllerSetCmd(dataDir *string) *cobra.Command {
	var (
		publicAddr, domain, acmeEmail, acmeHTTP, nodeListen, httpListen string
		ephemeralTimeout                                                time.Duration
		mdns, restart                                                   bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Change controller settings (run on the controller machine, with sudo)",
		Long: `Change controller settings and restart the controller. Without flags it
prints the current settings. Workers that already joined keep working:
they trust the controller's CA, not its name or address.`,
		Example: `  sudo cluster controller set --public-addr controller1.ntf307.com
  sudo cluster controller set --domain controller1.ntf307.com --acme-email me@example.com
  sudo cluster controller set --ephemeral-timeout 30m`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := controller.LoadConfig(*dataDir)
			if err != nil {
				return err
			}
			f := cmd.Flags()
			changed := false
			set := func(name string, apply func()) {
				if f.Changed(name) {
					apply()
					changed = true
				}
			}
			set("public-addr", func() { cfg.PublicAddr = publicAddr })
			set("domain", func() { cfg.Domain = domain })
			set("acme-email", func() { cfg.ACMEEmail = acmeEmail })
			set("acme-http-listen", func() { cfg.ACMEHTTPListen = acmeHTTP })
			set("node-listen", func() { cfg.NodeListen = nodeListen })
			set("http-listen", func() { cfg.HTTPListen = httpListen })
			set("ephemeral-timeout", func() { cfg.EphemeralTimeoutSec = int(ephemeralTimeout.Seconds()) })
			set("mdns", func() { cfg.DisableMDNS = !mdns })

			if changed {
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("save settings (run with sudo?): %w", err)
				}
				fmt.Println("Settings saved.")
			}
			printControllerSettings(cfg)
			if !changed {
				return nil
			}
			if f.Changed("domain") && cfg.Domain != "" {
				if _, port, _ := net.SplitHostPort(cfg.HTTPListen); port != "443" && cfg.ACMEHTTPListen == "" {
					fmt.Printf("Note: Let's Encrypt checks the domain on port 443. Forward port 443 on your router to\n"+
						"this machine's port %s (or use --http-listen :443), or add --acme-http-listen :80 and forward port 80.\n", port)
				}
			}
			if restart && serviceActive("cluster-controller") {
				out, err := exec.Command("systemctl", "restart", "cluster-controller").CombinedOutput()
				if err != nil {
					return fmt.Errorf("restart failed: %v: %s", err, out)
				}
				fmt.Println("Controller restarted.")
			} else {
				fmt.Println("Restart the controller for the changes to take effect.")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&publicAddr, "public-addr", "", "name or IP workers and browsers use (empty to clear)")
	f.StringVar(&domain, "domain", "", "get a Let's Encrypt certificate for the web UI for this domain (empty to turn off)")
	f.StringVar(&acmeEmail, "acme-email", "", "contact email for Let's Encrypt (optional)")
	f.StringVar(&acmeHTTP, "acme-http-listen", "", `also answer Let's Encrypt on this address, e.g. ":80" (needs port 80 forwarded)`)
	f.StringVar(&nodeListen, "node-listen", "", "address workers connect to, e.g. :7443")
	f.StringVar(&httpListen, "http-listen", "", "address of the web UI and API, e.g. :8443 or :443")
	f.DurationVar(&ephemeralTimeout, "ephemeral-timeout", 0, "remove ephemeral nodes offline this long (0 = never)")
	f.BoolVar(&mdns, "mdns", true, "advertise the controller on the LAN")
	f.BoolVar(&restart, "restart", true, "restart the controller service afterwards")
	return cmd
}

func printControllerSettings(cfg *controller.Config) {
	t := newTable()
	row := func(k, v string) {
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(t, "  %s\t%s\n", k, v)
	}
	row("Worker address", cfg.NodeAddr())
	row("Web UI", cfg.UIURL())
	row("public-addr", cfg.PublicAddr)
	row("node-listen", cfg.NodeListen)
	row("http-listen", cfg.HTTPListen)
	row("domain (Let's Encrypt)", cfg.Domain)
	row("acme-email", cfg.ACMEEmail)
	row("acme-http-listen", cfg.ACMEHTTPListen)
	ttl := "never"
	if cfg.EphemeralTimeoutSec > 0 {
		ttl = (time.Duration(cfg.EphemeralTimeoutSec) * time.Second).String()
	}
	row("ephemeral-timeout", ttl)
	row("mdns", fmt.Sprint(!cfg.DisableMDNS))
	t.Flush()
}

func serviceActive(name string) bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", name).Run() == nil
}
