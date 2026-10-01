package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/NTFespolion307/QuorvexFusion/internal/api"
	"github.com/NTFespolion307/QuorvexFusion/internal/client"
	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
)

const defaultControllerDir = "/var/lib/cluster"

func controllerCmd() *cobra.Command {
	var dataDir, nodeListen, httpListen, publicAddr string

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the controller (use `controller init` first)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := controller.LoadConfig(dataDir)
			if err != nil {
				return err
			}
			// Flags given explicitly override controller.json.
			if cmd.Flags().Changed("node-listen") {
				cfg.NodeListen = nodeListen
			}
			if cmd.Flags().Changed("http-listen") {
				cfg.HTTPListen = httpListen
			}
			if cmd.Flags().Changed("public-addr") {
				cfg.PublicAddr = publicAddr
			}
			log := newLogger()
			c, err := controller.New(cfg, log)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return c.Run(ctx, api.New(c, log).Handler())
		},
	}
	f := cmd.PersistentFlags()
	f.StringVar(&dataDir, "data-dir", envOr("CLUSTER_DATA_DIR", defaultControllerDir), "controller data directory")
	cmd.Flags().StringVar(&nodeListen, "node-listen", ":7443", "address workers connect to")
	cmd.Flags().StringVar(&httpListen, "http-listen", ":8443", "address of the web UI and API")
	cmd.Flags().StringVar(&publicAddr, "public-addr", "", "host name or IP that workers and browsers use to reach this controller")

	cmd.AddCommand(controllerInitCmd(&dataDir), controllerPasswdCmd(&dataDir))
	return cmd
}

func controllerInitCmd(dataDir *string) *cobra.Command {
	var nodeListen, httpListen, publicAddr, cliConfig string
	var passwordStdin, noMDNS bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the controller's data directory, CA, admin password and first join token",
		RunE: func(cmd *cobra.Command, args []string) error {
			password := os.Getenv("CLUSTER_ADMIN_PASSWORD")
			var err error
			switch {
			case passwordStdin:
				password, err = readStdinLine()
			case password == "":
				password, err = readPassword("Admin password (min 8 chars): ", true)
			}
			if err != nil {
				return err
			}

			cfg := controller.DefaultConfig(*dataDir)
			cfg.NodeListen, cfg.HTTPListen, cfg.PublicAddr = nodeListen, httpListen, publicAddr
			cfg.DisableMDNS = noMDNS
			res, err := controller.Init(cfg, password)
			if err != nil {
				return err
			}

			// Point the local CLI at this controller.
			host, port, _ := net.SplitHostPort(cfg.HTTPListen)
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			caPEM, err := os.ReadFile(*dataDir + "/ca.crt")
			if err != nil {
				return err
			}
			if cliConfig == "" {
				cliConfig = client.DefaultConfigPath()
			}
			if err := client.SaveConfig(cliConfig, &client.Config{
				Controller: "https://" + net.JoinHostPort(host, port), Token: res.APIToken, CACert: string(caPEM),
			}); err != nil {
				return fmt.Errorf("write CLI config: %w", err)
			}

			if globalFlags.json {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{
					"data_dir": *dataDir, "ui_url": cfg.UIURL(), "node_addr": cfg.NodeAddr(),
					"ca_fingerprint": res.CAFingerprint, "join_token": res.JoinToken, "join_code": res.JoinCode,
					"cli_config": cliConfig,
				})
			}
			self := selfCommand()
			fmt.Printf(`Controller initialised in %s

  Web UI:          %s   (log in with the admin password)
  Worker address:  %s
  CA fingerprint:  %s
  CLI config:      %s

Join code (auto-approve, valid 7 days):  %s

Join a worker with:
  %s worker --controller %s --token %s

Start the controller with:
  %s controller --data-dir %s
`, *dataDir, cfg.UIURL(), cfg.NodeAddr(), res.CAFingerprint, cliConfig,
				res.JoinCode, self, cfg.NodeAddr(), res.JoinCode, self, *dataDir)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&nodeListen, "node-listen", ":7443", "address workers connect to")
	f.StringVar(&httpListen, "http-listen", ":8443", "address of the web UI and API")
	f.StringVar(&publicAddr, "public-addr", "", "host name or IP that workers and browsers use (domain, public IP, Tailscale name)")
	f.StringVar(&cliConfig, "cli-config", "", "where to write the local CLI config (default ~/.config/cluster/cli.json)")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the admin password from stdin (or set CLUSTER_ADMIN_PASSWORD)")
	f.BoolVar(&noMDNS, "no-mdns", false, "don't advertise the controller on the LAN")
	return cmd
}

func controllerPasswdCmd(dataDir *string) *cobra.Command {
	var passwordStdin bool
	cmd := &cobra.Command{
		Use:   "passwd",
		Short: "Change the admin password (run on the controller machine)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var pw string
			var err error
			if passwordStdin {
				pw, err = readStdinLine()
			} else {
				pw, err = readPassword("New admin password: ", true)
			}
			if err != nil {
				return err
			}
			if err := controller.SetAdminPassword(*dataDir, pw); err != nil {
				return err
			}
			fmt.Println("Admin password changed.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	return cmd
}
