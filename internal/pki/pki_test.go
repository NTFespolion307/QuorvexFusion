package pki

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestNodeCertChainsToCA(t *testing.T) {
	ca, err := CreateCA("test CA")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := NewKey()
	csrDER, err := CreateCSR(key, "host1")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := ParseCSR(csrDER)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.SignNodeCSR(csr, "n1234")
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "n1234" {
		t.Errorf("CN = %q, want node ID", cert.Subject.CommonName)
	}
	if err := cert.CheckSignatureFrom(ca.Cert); err != nil {
		t.Errorf("node cert not signed by CA: %v", err)
	}
}

func TestParseCSRRejectsTampering(t *testing.T) {
	key, _ := NewKey()
	der, _ := CreateCSR(key, "host1")
	der[len(der)-1] ^= 0xff // corrupt the signature
	if _, err := ParseCSR(der); err == nil {
		t.Fatal("tampered CSR accepted")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	want := "sha256:abcdef"
	for _, in := range []string{"sha256:abcdef", "ABCDEF", " ab:cd:ef ", "SHA256:AB:CD:EF"} {
		if got := NormalizeFingerprint(in); got != want {
			t.Errorf("NormalizeFingerprint(%q) = %q", in, got)
		}
	}
}

// startTLSServer serves TLS with a certificate from ca and returns its address.
func startTLSServer(t *testing.T, ca *CA) string {
	t.Helper()
	cert, err := ca.IssueServerCert([]string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.(*tls.Conn).Handshake()
				c.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestPinnedTLSAndFetchCA(t *testing.T) {
	ca, _ := CreateCA("right")
	other, _ := CreateCA("wrong")
	addr := startTLSServer(t, ca)

	got, err := FetchCA(addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(got) != ca.Fingerprint() {
		t.Fatal("FetchCA returned a different CA")
	}

	// Connecting by IP works even though the cert only names "localhost":
	// pinning replaces host name checks.
	conn, err := tls.Dial("tcp", addr, PinnedClientTLS(ca.Cert, nil))
	if err != nil {
		t.Fatalf("pinned dial to the right CA failed: %v", err)
	}
	conn.Close()

	if conn, err := tls.Dial("tcp", addr, PinnedClientTLS(other.Cert, nil)); err == nil {
		conn.Close()
		t.Fatal("pinned dial accepted a server from another CA")
	}
}
