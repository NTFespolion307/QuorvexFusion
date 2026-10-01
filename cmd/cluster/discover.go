package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/NTFespolion307/QuorvexFusion/internal/discovery"
)

func discoverCmd() *cobra.Command {
	var timeout time.Duration
	var plain bool
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find controllers on the local network (mDNS)",
		RunE: func(cmd *cobra.Command, args []string) error {
			found, err := discovery.Browse(timeout)
			if err != nil {
				return err
			}
			switch {
			case globalFlags.json:
				return printJSON(found)
			case plain:
				// One controller per line: address, fingerprint, name
				// (tab-separated, for install.sh).
				for _, c := range found {
					fmt.Printf("%s\t%s\t%s\n", c.Addr, c.Fingerprint, c.Name)
				}
				return nil
			}
			if len(found) == 0 {
				fmt.Fprintln(os.Stderr, "No controllers found. Multicast may be blocked (VPNs, cloud networks); pass the address directly.")
				return nil
			}
			t := newTable()
			fmt.Fprintln(t, "NAME\tADDRESS\tVERSION\tCA FINGERPRINT")
			for _, c := range found {
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", c.Name, c.Addr, c.Version, c.Fingerprint)
			}
			return t.Flush()
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "how long to listen")
	cmd.Flags().BoolVar(&plain, "plain", false, "tab-separated output: address, fingerprint, name")
	return cmd
}
