package controlplane

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"
)

// NodeCertSigner signs node client certificates.
type NodeCertSigner struct {
	caCert *x509.Certificate
	caKey  crypto.Signer
	ttl    time.Duration
}

// LoadNodeCertSigner loads CA material and returns a signer.
func LoadNodeCertSigner(caCertFile, caKeyFile string, ttl time.Duration) (*NodeCertSigner, error) {
	certPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("reading ca cert: %w", err)
	}
	keyPEM, err := os.ReadFile(caKeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading ca key: %w", err)
	}

	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("decoding ca cert pem")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing ca cert: %w", err)
	}

	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("decoding ca key pem")
	}

	var signer crypto.Signer
	var parseErrs []string
	if key, err := x509.ParsePKCS8PrivateKey(kb.Bytes); err == nil {
		if s, ok := key.(crypto.Signer); ok {
			signer = s
		} else {
			parseErrs = append(parseErrs, "pkcs8: key does not implement crypto.Signer")
		}
	} else {
		parseErrs = append(parseErrs, fmt.Sprintf("pkcs8: %v", err))
	}
	if signer == nil {
		if key, err := x509.ParsePKCS1PrivateKey(kb.Bytes); err == nil {
			signer = key
		} else {
			parseErrs = append(parseErrs, fmt.Sprintf("pkcs1: %v", err))
		}
	}
	if signer == nil {
		if key, err := x509.ParseECPrivateKey(kb.Bytes); err == nil {
			signer = key
		} else {
			parseErrs = append(parseErrs, fmt.Sprintf("ec: %v", err))
		}
	}
	if signer == nil {
		return nil, fmt.Errorf("unsupported ca key type; parse attempts: %s", strings.Join(parseErrs, "; "))
	}

	return &NodeCertSigner{
		caCert: cert,
		caKey:  signer,
		ttl:    ttl,
	}, nil
}

// SignCSR signs a node CSR and returns leaf cert PEM plus expiry.
func (s *NodeCertSigner) SignCSR(nodeID string, csrPEM string) (certPEM string, expiresAt time.Time, err error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		return "", time.Time{}, fmt.Errorf("decoding csr pem")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parsing csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", time.Time{}, fmt.Errorf("verifying csr signature: %w", err)
	}

	uri, err := url.Parse("spiffe://firework/nodes/" + nodeID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("building node uri: %w", err)
	}

	now := time.Now().UTC()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generating serial: %w", err)
	}

	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: strings.TrimSpace(nodeID),
		},
		NotBefore:             now.Add(-1 * time.Minute),
		NotAfter:              now.Add(s.ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{uri},
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signing certificate: %w", err)
	}

	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return string(certPEMBytes), tpl.NotAfter.UTC(), nil
}

func nodeIDFromCertificate(cert *x509.Certificate) string {
	const spiffePrefix = "spiffe://firework/nodes/"
	for _, u := range cert.URIs {
		s := u.String()
		if strings.HasPrefix(s, spiffePrefix) {
			return strings.TrimPrefix(s, spiffePrefix)
		}
	}
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	return ""
}
