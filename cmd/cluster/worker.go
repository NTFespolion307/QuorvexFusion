package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NTFespolion307/QuorvexFusion/internal/hw"
	"github.com/NTFespolion307/QuorvexFusion/internal/worker"
)

const defaultWorkerDir = "/var/lib/cluster-worker"

// exitRevoked tells process supervisors not to restart a revoked worker
// (the systemd unit sets RestartPreventExitStatus=3).
const exitRevoked = 3

type workerFlags struct {
	dataDir, controller, token, code, fingerprint string
	name, location, sharedStorage, taskUser       string
	ephemeral, yes                                bool
	labels                                        map[string]string
}

func (wf *workerFlags) register(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	// Environment variables make the Docker image and cloud-init easy to configure.
	f.StringVar(&wf.dataDir, "data-dir", envOr("CLUSTER_WORKER_DATA_DIR", defaultWorkerDir), "worker data directory (identity and cache)")
	f.StringVar(&wf.controller, "controller", os.Getenv("CLUSTER_WORKER_CONTROLLER"), "controller address host:port (node port, default 7443)")
	f.StringVar(&wf.token, "token", os.Getenv("CLUSTER_JOIN_TOKEN"), "join code or token (only needed the first time)")
	f.StringVar(&wf.code, "code", "", "join code, e.g. 7KQ2-MX4P-9TRA-BH3W-C8NE (same as --token)")
	f.StringVar(&wf.fingerprint, "ca-fingerprint", os.Getenv("CLUSTER_CA_FINGERPRINT"), "expected controller CA fingerprint (sha256:...)")
	f.StringVar(&wf.name, "name", os.Getenv("CLUSTER_NODE_NAME"), "node name (default: hostname)")
	f.StringVar(&wf.location, "location", os.Getenv("CLUSTER_LOCATION"), "location label, e.g. home, vastai, gcp-us-central1")
	f.StringVar(&wf.sharedStorage, "shared-storage", os.Getenv("CLUSTER_SHARED_STORAGE"), "path of storage shared with the controller (skips file transfers)")
	f.BoolVar(&wf.ephemeral, "ephemeral", os.Getenv("CLUSTER_EPHEMERAL") == "1", "mark this node as ephemeral (cloud/rented)")
	f.StringVar(&wf.taskUser, "task-user", os.Getenv("CLUSTER_TASK_USER"), "run tasks as this user (default: 'cluster' if it exists and the worker is root)")
	f.BoolVar(&wf.yes, "yes", false, "trust the controller's CA without a prompt if no --ca-fingerprint is given")
	f.StringToStringVar(&wf.labels, "label", nil, "node label key=value (repeatable)")
}

func (wf *workerFlags) options() worker.Options {
	if wf.code != "" {
		wf.token = wf.code
	}
	// CLUSTER_LABELS="gpu=4090,zone=home" (e.g. from /etc/cluster/worker.env)
	// is merged under any --label flags.
	if env := os.Getenv("CLUSTER_LABELS"); env != "" {
		merged := map[string]string{}
		for _, kv := range strings.Split(env, ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && k != "" {
				merged[k] = v
			}
		}
		for k, v := range wf.labels {
			merged[k] = v
		}
		wf.labels = merged
	}
	return worker.Options{
		DataDir: wf.dataDir, Controller: wf.controller, Token: wf.token, CAFingerprint: wf.fingerprint,
		ConfirmFingerprint: fingerprintConfirmer(wf.yes),
		Name:               wf.name, Location: wf.location, Ephemeral: wf.ephemeral,
		Labels: wf.labels, SharedStorage: wf.sharedStorage, TaskUser: wf.taskUser,
		Log: newLogger(),
	}
}

func workerCmd() *cobra.Command {
	var wf workerFlags
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run a worker: join the controller (first run) and contribute this machine to the pool",
		Long: `Run a worker in the foreground. On the first run it joins using --token and
saves its identity in --data-dir; later runs need no token.

  cluster worker --controller host:7443 --token cjt_... --ca-fingerprint sha256:...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err := worker.New(wf.options()).Run(ctx)
			if errors.Is(err, worker.ErrRevoked) {
				return &exitError{code: exitRevoked, err: err}
			}
			return err
		},
	}
	wf.register(cmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "probe",
		Short: "Print the hardware and metrics this worker would report, then exit",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := hw.Probe()
			s := hw.NewSampler(info)
			s.Sample()
			time.Sleep(time.Second) // CPU and network figures are rates over this interval
			m := s.Sample()
			out, err := protojson.MarshalOptions{Multiline: true, UseProtoNames: true}.Marshal(info)
			if err != nil {
				return err
			}
			fmt.Printf("hardware: %s\n", out)
			out, err = protojson.MarshalOptions{Multiline: true, UseProtoNames: true}.Marshal(m)
			if err != nil {
				return err
			}
			fmt.Printf("metrics: %s\n", out)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "join",
		Short: "Join the controller and exit (used by install.sh before starting the service)",
		RunE: func(cmd *cobra.Command, args []string) error {
			nodeID, pending, err := worker.New(wf.options()).JoinOnly(cmd.Context())
			if err != nil {
				return err
			}
			if pending {
				fmt.Printf("Joined as node %s. Approval pending: approve it in the web UI or with `cluster nodes approve %s`.\n", nodeID, nodeID)
			} else {
				fmt.Printf("Joined as node %s (approved).\n", nodeID)
			}
			return nil
		},
	})
	return cmd
}
