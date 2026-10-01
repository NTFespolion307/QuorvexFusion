// Command cluster is the single binary for the whole system:
//
//	cluster controller ...   run (or initialise) the controller
//	cluster worker ...       run a worker that joins a controller
//	cluster <command> ...    CLI commands talking to the controller's API
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NTFespolion307/QuorvexFusion/internal/version"
)

// exitError carries a specific process exit code out of a command.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func main() {
	root := &cobra.Command{
		Use:           "cluster",
		Short:         "Self-hosted compute cluster: pool machines and run jobs on them",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&globalFlags.config, "config", "",
		"CLI config file (default ~/.config/cluster/cli.json, then /etc/cluster/cli.json)")
	root.PersistentFlags().BoolVar(&globalFlags.json, "json", false, "print raw JSON output")
	root.PersistentFlags().StringVar(&globalFlags.logLevel, "log-level", "info", "log level: debug, info, warn, error")

	root.AddCommand(
		controllerCmd(),
		workerCmd(),
		loginCmd(),
		statusCmd(),
		nodesCmd(),
		tokenCmd(),
		apiTokenCmd(),
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Run:   func(*cobra.Command, []string) { fmt.Println(version.Version) },
		},
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(1)
	}
}

var globalFlags struct {
	config   string
	json     bool
	logLevel string
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(globalFlags.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// envOr returns the environment variable's value, or def if unset.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
