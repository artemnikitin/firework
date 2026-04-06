package agent

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/artemnikitin/firework/internal/config"
)

func TestSelectHostIPv4_SkipsBridgeAndVirtualInterfaces(t *testing.T) {
	candidates := []hostIPInterface{
		{
			Name:  "br-firework",
			Flags: net.FlagUp,
			Addrs: []net.Addr{ipAddr("172.16.0.1")},
		},
		{
			Name:  "veth1234",
			Flags: net.FlagUp,
			Addrs: []net.Addr{ipAddr("10.250.0.2")},
		},
		{
			Name:  "eth0",
			Flags: net.FlagUp,
			Addrs: []net.Addr{ipAddr("10.0.1.25")},
		},
	}

	got := selectHostIPv4(candidates, "br-firework")
	if got != "10.0.1.25" {
		t.Fatalf("expected host ip from physical interface, got %q", got)
	}
}

func TestSelectHostIPv4_UsesConfiguredVMBridgeFilter(t *testing.T) {
	candidates := []hostIPInterface{
		{
			Name:  "fw-node-bridge",
			Flags: net.FlagUp,
			Addrs: []net.Addr{ipAddr("172.16.0.1")},
		},
		{
			Name:  "ens3",
			Flags: net.FlagUp,
			Addrs: []net.Addr{ipAddr("10.10.0.8")},
		},
	}

	got := selectHostIPv4(candidates, "fw-node-bridge")
	if got != "10.10.0.8" {
		t.Fatalf("expected vm_bridge interface to be skipped, got %q", got)
	}
}

func TestMTLSHTTPClient_ReusesClientInstance(t *testing.T) {
	caFile := writeTestCA(t)
	c := &registryClient{
		cfg: config.AgentConfig{
			RegistryCAFile:     caFile,
			RegistryServerName: "registry.internal",
			RegistryCertFile:   "/tmp/node.crt",
			RegistryKeyFile:    "/tmp/node.key",
		},
		logger: testLogger(),
	}

	first, err := c.mtlsHTTPClient()
	if err != nil {
		t.Fatalf("creating first mTLS client: %v", err)
	}
	second, err := c.mtlsHTTPClient()
	if err != nil {
		t.Fatalf("creating second mTLS client: %v", err)
	}
	if first != second {
		t.Fatal("expected mtlsHTTPClient to reuse existing client instance")
	}
}

func ipAddr(raw string) net.Addr {
	return &net.IPNet{
		IP:   net.ParseIP(raw),
		Mask: net.CIDRMask(24, 32),
	}
}

func writeTestCA(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating test ca cert: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemData, 0o644); err != nil {
		t.Fatalf("writing test ca cert: %v", err)
	}
	return path
}
