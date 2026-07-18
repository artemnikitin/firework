package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/artemnikitin/firework/internal/capacity"
	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/statusmodel"
)

type registryClient struct {
	cfg        config.AgentConfig
	logger     *slog.Logger
	generation int64
	hostIP     string

	mu         sync.Mutex
	registered bool
	certExpiry time.Time
	mtlsClient *http.Client
}

func newRegistryClient(cfg config.AgentConfig, logger *slog.Logger, generation int64) *registryClient {
	return &registryClient{
		cfg:        cfg,
		logger:     logger,
		generation: generation,
		hostIP:     detectHostIP(cfg.RegistryURL, cfg.VMBridge),
	}
}

func (c *registryClient) sync(ctx context.Context, nodeID string, labels []string, cap, used capacity.NodeCapacity, status *statusmodel.AgentStatus) {
	if err := c.ensureCertificate(ctx, nodeID); err != nil {
		c.logger.Warn("registry certificate setup failed", "error", err)
		return
	}
	if !c.isRegistered() {
		if err := c.register(ctx, nodeID, labels, cap); err != nil {
			c.logger.Warn("registry register failed", "error", err)
			return
		}
		c.setRegistered(true)
	}
	if err := c.heartbeat(ctx, nodeID, cap, used, status); err != nil {
		c.logger.Warn("registry heartbeat failed", "error", err)
		c.setRegistered(false)
	}
}

type registerRequest struct {
	NodeID     string         `json:"node_id"`
	Generation int64          `json:"generation"`
	Labels     []string       `json:"labels,omitempty"`
	Capacity   capPayload     `json:"capacity"`
	State      string         `json:"state"`
	HostIP     string         `json:"host_ip,omitempty"`
	Storage    storagePayload `json:"storage,omitempty"`
}

type heartbeatRequest struct {
	NodeID      string                   `json:"node_id"`
	Generation  int64                    `json:"generation"`
	Capacity    capPayload               `json:"capacity"`
	Used        capPayload               `json:"used"`
	HostIP      string                   `json:"host_ip,omitempty"`
	AgentStatus *statusmodel.AgentStatus `json:"agent_status,omitempty"`
	Storage     storagePayload           `json:"storage,omitempty"`
}

type capPayload struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
}

type storagePayload struct {
	LocalCapacityBytes  int64  `json:"local_capacity_bytes,omitempty"`
	SharedBackendID     string `json:"shared_backend_id,omitempty"`
	SharedCapacityBytes int64  `json:"shared_capacity_bytes,omitempty"`
}

type certRequest struct {
	NodeID         string `json:"node_id,omitempty"`
	BootstrapToken string `json:"bootstrap_token,omitempty"`
	CSRPEM         string `json:"csr_pem"`
}

type certResponse struct {
	CertPEM   string    `json:"cert_pem"`
	ExpiresAt time.Time `json:"expires_at"`
}

type stateRequest struct {
	State string `json:"state"`
}

const (
	nodeStateDown = "down"
)

func (c *registryClient) register(ctx context.Context, nodeID string, labels []string, cap capacity.NodeCapacity) error {
	req := registerRequest{
		NodeID:     nodeID,
		Generation: c.generation,
		Labels:     labels,
		Capacity: capPayload{
			VCPUs:    cap.VCPUs,
			MemoryMB: cap.MemoryMB,
		},
		State:   "ready",
		HostIP:  c.hostIP,
		Storage: c.storagePayload(),
	}
	return c.postMTLS(ctx, "/v1/nodes/register", req, nil)
}

func (c *registryClient) heartbeat(ctx context.Context, nodeID string, cap, used capacity.NodeCapacity, status *statusmodel.AgentStatus) error {
	req := heartbeatRequest{
		NodeID:     nodeID,
		Generation: c.generation,
		Capacity: capPayload{
			VCPUs:    cap.VCPUs,
			MemoryMB: cap.MemoryMB,
		},
		Used: capPayload{
			VCPUs:    used.VCPUs,
			MemoryMB: used.MemoryMB,
		},
		HostIP:      c.hostIP,
		AgentStatus: status,
		Storage:     c.storagePayload(),
	}
	return c.postMTLS(ctx, "/v1/nodes/heartbeat", req, nil)
}

func (c *registryClient) storagePayload() storagePayload {
	var out storagePayload
	if c.cfg.Storage.Local != nil {
		out.LocalCapacityBytes = c.cfg.Storage.Local.CapacityBytes
	}
	if c.cfg.Storage.Shared != nil {
		out.SharedBackendID = c.cfg.Storage.Shared.BackendID
		out.SharedCapacityBytes = c.cfg.Storage.Shared.CapacityBytes
	}
	return out
}

func (c *registryClient) ensureCertificate(ctx context.Context, nodeID string) error {
	now := time.Now().UTC()
	bootstrapToken := strings.TrimSpace(c.cfg.RegistryBootstrapToken)
	c.mu.Lock()
	certExpiry := c.certExpiry
	c.mu.Unlock()
	if certExpiry.After(now.Add(c.cfg.RegistryCertRenewBefore)) {
		return nil
	}

	cert, err := loadLeafCert(c.cfg.RegistryCertFile)
	hasCert := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("loading registry cert: %w", err)
	}
	if !hasCert && bootstrapToken == "" {
		return fmt.Errorf("registry cert missing and registry_bootstrap_token is empty; provide a bootstrap token or pre-provision node cert/key")
	}
	if hasCert {
		if cert.NotAfter.After(now.Add(c.cfg.RegistryCertRenewBefore)) {
			c.mu.Lock()
			c.certExpiry = cert.NotAfter.UTC()
			c.mu.Unlock()
			return nil
		}
	}

	priv, err := loadOrCreatePrivateKey(c.cfg.RegistryKeyFile)
	if err != nil {
		return err
	}

	csrPEM, err := buildCSR(nodeID, priv)
	if err != nil {
		return err
	}

	var out certResponse
	if hasCert {
		err = c.postMTLS(ctx, "/v1/nodes/renew", certRequest{CSRPEM: csrPEM}, &out)
		if err != nil && shouldFallbackToEnroll(err) {
			// Renew is identity-bound; if TLS/router rejects current cert, recover
			// with a fresh enrollment only when a bootstrap token is available.
			if bootstrapToken == "" {
				return fmt.Errorf("registry cert renew rejected and registry_bootstrap_token is empty; configure bootstrap token or pre-provision a fresh cert/key: %w", err)
			}
			err = c.postEnroll(ctx, certRequest{
				NodeID:         nodeID,
				BootstrapToken: bootstrapToken,
				CSRPEM:         csrPEM,
			}, &out)
		}
	} else {
		err = c.postEnroll(ctx, certRequest{
			NodeID:         nodeID,
			BootstrapToken: bootstrapToken,
			CSRPEM:         csrPEM,
		}, &out)
	}
	if err != nil {
		return err
	}

	if strings.TrimSpace(out.CertPEM) == "" {
		return fmt.Errorf("registry returned empty cert_pem")
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.RegistryCertFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(c.cfg.RegistryCertFile, []byte(out.CertPEM), 0o600); err != nil {
		return fmt.Errorf("writing registry cert file: %w", err)
	}

	cert, err = loadLeafCert(c.cfg.RegistryCertFile)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.certExpiry = cert.NotAfter.UTC()
	c.mu.Unlock()
	return nil
}

func (c *registryClient) postEnroll(ctx context.Context, p any, out any) error {
	client, err := c.enrollHTTPClient()
	if err != nil {
		return err
	}
	return doJSONRequest(ctx, client, http.MethodPost, c.cfg.RegistryURL+"/v1/nodes/enroll", p, out)
}

func (c *registryClient) postMTLS(ctx context.Context, path string, p any, out any) error {
	client, err := c.mtlsHTTPClient()
	if err != nil {
		return err
	}
	return doJSONRequest(ctx, client, http.MethodPost, c.cfg.RegistryURL+path, p, out)
}

func (c *registryClient) markDown(ctx context.Context, nodeID string) {
	if err := c.setState(ctx, nodeID, nodeStateDown); err != nil {
		c.logger.Warn("registry node state update failed during shutdown", "state", nodeStateDown, "error", err)
	}
}

func (c *registryClient) setState(ctx context.Context, nodeID, state string) error {
	if strings.TrimSpace(nodeID) == "" {
		return fmt.Errorf("node id is required")
	}
	req := stateRequest{State: state}
	return c.postMTLS(ctx, "/v1/nodes/"+url.PathEscape(nodeID)+"/state", req, nil)
}

func (c *registryClient) enrollHTTPClient() (*http.Client, error) {
	roots, err := c.registryRoots()
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: c.cfg.RegistryServerName,
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}, nil
}

func (c *registryClient) mtlsHTTPClient() (*http.Client, error) {
	c.mu.Lock()
	if c.mtlsClient != nil {
		client := c.mtlsClient
		c.mu.Unlock()
		return client, nil
	}
	c.mu.Unlock()

	roots, err := c.registryRoots()
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: c.cfg.RegistryServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(c.cfg.RegistryCertFile, c.cfg.RegistryKeyFile)
			if err != nil {
				return nil, fmt.Errorf("loading node client cert: %w", err)
			}
			return &cert, nil
		},
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mtlsClient == nil {
		c.mtlsClient = client
	}
	return c.mtlsClient, nil
}

func (c *registryClient) registryRoots() (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(c.cfg.RegistryCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading registry_ca_file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parsing registry ca pem")
	}
	return roots, nil
}

func doJSONRequest(ctx context.Context, client *http.Client, method, url string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := ioReadAllLimit(resp.Body, 8*1024)
		return &requestError{
			StatusCode: resp.StatusCode,
			Body:       string(buf),
		}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type requestError struct {
	StatusCode int
	Body       string
}

func (e *requestError) Error() string {
	return fmt.Sprintf("registry request failed: status=%d body=%s", e.StatusCode, e.Body)
}

func shouldFallbackToEnroll(err error) bool {
	// The renew call reached registry and was rejected due to identity/auth issues.
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		return reqErr.StatusCode == http.StatusUnauthorized || reqErr.StatusCode == http.StatusForbidden
	}

	// TLS layer rejected the client cert before HTTP routing.
	var certInvalid x509.CertificateInvalidError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certInvalid) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) {
		return true
	}

	// Some TLS stacks surface certificate rejection as plain text without a
	// structured x509 error type.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "bad certificate") || strings.Contains(msg, "certificate has expired")
}

func ioReadAllLimit(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

func loadLeafCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("decoding cert pem")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return cert, nil
}

func loadOrCreatePrivateKey(path string) (ed25519.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("decoding key pem")
		}
		keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing pkcs8 key: %w", err)
		}
		key, ok := keyAny.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("registry key is not ed25519")
		}
		return key, nil
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	if err := os.WriteFile(path, keyPEM, 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

func buildCSR(nodeID string, key ed25519.PrivateKey) (string, error) {
	uri, err := url.Parse("spiffe://firework/nodes/" + nodeID)
	if err != nil {
		return "", err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName: nodeID,
		},
		URIs: []*url.URL{uri},
	}, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})), nil
}

func detectHostIP(registryURL, vmBridge string) string {
	if ip := detectHostIPByRoute(registryURL); ip != "" {
		return ip
	}
	return detectHostIPFromInterfaces(vmBridge)
}

func detectHostIPByRoute(registryURL string) string {
	if strings.TrimSpace(registryURL) == "" {
		return ""
	}
	u, err := url.Parse(registryURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(u.Scheme)
	}
	conn, err := net.DialTimeout("udp", net.JoinHostPort(host, port), 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	ip := udpAddr.IP.To4()
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return ""
	}
	return ip.String()
}

func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	default:
		return "443"
	}
}

type hostIPInterface struct {
	Name  string
	Flags net.Flags
	Addrs []net.Addr
}

func detectHostIPFromInterfaces(vmBridge string) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	candidates := make([]hostIPInterface, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		candidates = append(candidates, hostIPInterface{
			Name:  iface.Name,
			Flags: iface.Flags,
			Addrs: addrs,
		})
	}
	return selectHostIPv4(candidates, vmBridge)
}

func selectHostIPv4(candidates []hostIPInterface, vmBridge string) string {
	for _, iface := range candidates {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if shouldSkipHostIPInterface(iface.Name, vmBridge) {
			continue
		}
		for _, addr := range iface.Addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip = ip.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

func shouldSkipHostIPInterface(name, vmBridge string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	b := strings.ToLower(strings.TrimSpace(vmBridge))
	if n == "" {
		return true
	}
	if b != "" && n == b {
		return true
	}
	if n == "lo" {
		return true
	}
	for _, p := range []string{"br-", "veth", "virbr", "docker", "cni", "flannel", "tap"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

func (c *registryClient) isRegistered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registered
}

func (c *registryClient) setRegistered(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registered = v
}
