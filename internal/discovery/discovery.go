// Package discovery lets workers find controllers on the local network
// with mDNS (multicast DNS, as used by printers and Chromecasts).
//
// The controller advertises a "_cluster-ctl._tcp" service whose port is
// the node port and whose TXT record carries the CA fingerprint and web UI
// address. mDNS is unauthenticated, so the advertised fingerprint is only a
// convenience: the user still confirms it against the one the controller
// printed at install time.
package discovery

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

const serviceType = "_cluster-ctl._tcp"

// Advertisement is what a controller announces.
type Advertisement struct {
	Port        int      // node port workers connect to
	IPs         []net.IP // addresses to announce
	Fingerprint string
	UIURL       string
	Version     string
}

// Advertise announces the controller until the returned stop function is
// called.
func Advertise(ad Advertisement) (stop func(), err error) {
	host, _ := os.Hostname()
	if host == "" {
		host = "controller"
	}
	txt := []string{"fp=" + ad.Fingerprint, "ui=" + ad.UIURL, "v=" + ad.Version}
	// The library resolves our own host name if no IPs are given, which
	// fails in some containers, so we always pass them explicitly.
	svc, err := mdns.NewMDNSService(host, serviceType, "", host+".local.", ad.Port, ad.IPs, txt)
	if err != nil {
		return nil, err
	}
	srv, err := mdns.NewServer(&mdns.Config{Zone: svc, Logger: quietLogger()})
	if err != nil {
		return nil, err
	}
	return func() { _ = srv.Shutdown() }, nil
}

// Controller is a controller found on the network.
type Controller struct {
	Name        string `json:"name"`
	Addr        string `json:"addr"` // host:port for --controller
	Fingerprint string `json:"fingerprint"`
	UIURL       string `json:"ui_url"`
	Version     string `json:"version"`
}

// Browse listens for controller announcements for the given duration.
func Browse(timeout time.Duration) ([]Controller, error) {
	entries := make(chan *mdns.ServiceEntry, 32)
	params := mdns.DefaultParams(serviceType)
	params.Entries = entries
	params.Timeout = timeout
	params.DisableIPv6 = true
	params.Logger = quietLogger()

	found := map[string]Controller{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range entries {
			if e.AddrV4 == nil {
				continue
			}
			c := Controller{
				Name: strings.TrimSuffix(strings.TrimSuffix(e.Name, "."+serviceType+".local."), "."),
				Addr: net.JoinHostPort(e.AddrV4.String(), fmt.Sprint(e.Port)),
			}
			for _, f := range e.InfoFields {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "fp":
					c.Fingerprint = v
				case "ui":
					c.UIURL = v
				case "v":
					c.Version = v
				}
			}
			found[c.Addr] = c
		}
	}()
	err := mdns.Query(params)
	close(entries)
	<-done
	if err != nil {
		return nil, err
	}
	out := make([]Controller, 0, len(found))
	for _, c := range found {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out, nil
}

// quietLogger drops the library's chatty informational logging.
func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }
