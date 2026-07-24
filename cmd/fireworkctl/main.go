package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/artemnikitin/firework/internal/controlplane"
	"github.com/artemnikitin/firework/internal/version"
	"gopkg.in/yaml.v3"
)

type cliConfig struct {
	Endpoint  string `yaml:"endpoint"`
	CAFile    string `yaml:"ca_file"`
	TokenFile string `yaml:"token_file"`
}

type apiClient struct {
	endpoint string
	token    string
	http     *http.Client
}

var (
	nodeStates    = []string{"ready", "draining", "down", "stale", "unknown"}
	serviceStates = []string{"pending", "running", "stopped", "failed", "unknown"}
	serviceHealth = []string{"healthy", "unhealthy", "unknown", "not_configured"}
)

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("API request failed (%d): %s", e.status, e.body)
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var statusErr *httpStatusError
		switch {
		case errors.As(err, &statusErr) && statusErr.status == http.StatusUnauthorized:
			os.Exit(3)
		case errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound:
			os.Exit(4)
		default:
			os.Exit(1)
		}
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || globalHelpRequested(args) {
		printUsage(out)
		return nil
	}
	if versionRequested(args) {
		fmt.Fprintln(out, "fireworkctl", version.String())
		return nil
	}
	if command, ok := subcommandHelp(args); ok {
		printCommandUsage(out, command)
		return nil
	}
	configPath := configPathFromArgs(args)
	cfg, err := loadCLIConfig(configPath)
	if err != nil {
		return err
	}
	applyEnv(&cfg)

	global := flag.NewFlagSet("fireworkctl", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	global.StringVar(&configPath, "config", configPath, "configuration file")
	global.StringVar(&cfg.Endpoint, "endpoint", cfg.Endpoint, "control-plane API endpoint")
	global.StringVar(&cfg.CAFile, "ca-file", cfg.CAFile, "CA bundle for API TLS")
	global.StringVar(&cfg.TokenFile, "token-file", cfg.TokenFile, "operator token file")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(out)
			return nil
		}
		return usageError(err.Error())
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		printUsage(out)
		return nil
	}
	command, commandArgs := remaining[0], remaining[1:]
	switch command {
	case "nodes":
		return runNodes(cfg, commandArgs, out)
	case "node":
		return runNode(cfg, commandArgs, out)
	case "services":
		return runServices(cfg, commandArgs, out)
	case "service":
		return runService(cfg, commandArgs, out)
	default:
		return usageError("unknown command " + command)
	}
}

func runNodes(cfg cliConfig, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("nodes", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	state := flags.String("state", "", "filter by state")
	output := flags.String("output", "table", "table or json")
	watch := flags.Duration("watch", 0, "polling interval")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return usageError("usage: fireworkctl nodes [--state STATE] [--output table|json] [--watch 5s]")
	}
	if err := validateFilter("state", *state, nodeStates); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateWatch(*watch); err != nil {
		return err
	}
	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}
	return poll(out, *watch, *output == "table", func() error {
		var response controlplane.ListEnvelope[controlplane.NodeSummary]
		if err := client.get(context.Background(), "/v1/nodes?"+url.Values{"state": []string{*state}}.Encode(), &response); err != nil {
			return err
		}
		if *output == "json" {
			return writeOutputJSON(out, response, *watch > 0)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NODE\tSTATE\tLAST SEEN\tSERVICES\tVCPU\tMEMORY")
		for _, node := range response.Items {
			lastSeen := formatAge(node.StatusAgeSeconds, node.LastSeenAt.IsZero())
			if lastSeen != "unknown" {
				lastSeen += " ago"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%d/%d\t%d/%d MB\n", node.NodeID, node.State, lastSeen, node.RunningServices, node.DesiredServices, node.Allocated.VCPUs, node.Capacity.VCPUs, node.Allocated.MemoryMB, node.Capacity.MemoryMB)
		}
		return w.Flush()
	})
}

func runNode(cfg cliConfig, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("node", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", "table", "table or json")
	watch := flags.Duration("watch", 0, "polling interval")
	if err := flags.Parse(reorderDetailArgs(args)); err != nil || flags.NArg() != 1 {
		return usageError("usage: fireworkctl node <node-id> [--output table|json] [--watch 5s]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateWatch(*watch); err != nil {
		return err
	}
	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}
	id := flags.Arg(0)
	return poll(out, *watch, *output == "table", func() error {
		var response controlplane.NodeDetail
		if err := client.get(context.Background(), "/v1/nodes/"+url.PathEscape(id), &response); err != nil {
			return err
		}
		if *output == "json" {
			return writeOutputJSON(out, response, *watch > 0)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "NODE\t%s\nSTATE\t%s\nRECONCILIATION\t%s\nLAST SEEN\t%s\nSTATUS AGE\t%s\nAGENT\t%s\nHOST IP\t%s\nREVISION\t%s\nSERVICES\t%d/%d running\nVCPU\t%d/%d allocated (%d available)\nMEMORY\t%d/%d MB allocated (%d MB available)\nSTATUS MISSING\t%s\nSTATUS STALE\t%s\nREASON\t%s\nMESSAGE\t%s\n", response.NodeID, response.State, valueOrUnknown(response.Reconciliation), formatTime(response.LastSeenAt), formatAge(response.StatusAgeSeconds, response.LastSeenAt.IsZero()), valueOrUnknown(response.AgentVersion), valueOrDash(response.HostIP), valueOrUnknown(response.AppliedRevision), response.RunningServices, response.DesiredServices, response.Allocated.VCPUs, response.Capacity.VCPUs, response.Available.VCPUs, response.Allocated.MemoryMB, response.Capacity.MemoryMB, response.Available.MemoryMB, formatBool(response.StatusMissing), formatBool(response.StatusStale), valueOrDash(response.ReasonCode), valueOrDash(response.Message))
		if len(response.Services) > 0 {
			fmt.Fprintln(w, "\nSERVICE\tSTATE\tHEALTH")
			for _, service := range response.Services {
				fmt.Fprintf(w, "%s\t%s\t%s\n", service.Name, service.State, service.Health)
			}
		}
		if len(response.Conditions) > 0 {
			fmt.Fprintln(w, "\nCONDITION\tSTATUS\tREASON\tMESSAGE\tLAST TRANSITION")
			for _, condition := range response.Conditions {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", condition.Type, condition.Status, valueOrDash(condition.ReasonCode), valueOrDash(condition.Message), formatTime(condition.LastTransitionAt))
			}
		}
		return w.Flush()
	})
}

func runServices(cfg cliConfig, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("services", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	state := flags.String("state", "", "filter by state")
	health := flags.String("health", "", "filter by health")
	node := flags.String("node", "", "filter by node")
	output := flags.String("output", "table", "table or json")
	watch := flags.Duration("watch", 0, "polling interval")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return usageError("usage: fireworkctl services [--state STATE] [--health HEALTH] [--node NODE] [--output table|json] [--watch 5s]")
	}
	if err := validateFilter("state", *state, serviceStates); err != nil {
		return err
	}
	if err := validateFilter("health", *health, serviceHealth); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateWatch(*watch); err != nil {
		return err
	}
	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}
	return poll(out, *watch, *output == "table", func() error {
		query := url.Values{"state": []string{*state}, "health": []string{*health}, "node": []string{*node}}
		var response controlplane.ListEnvelope[controlplane.ServiceSummary]
		if err := client.get(context.Background(), "/v1/services?"+query.Encode(), &response); err != nil {
			return err
		}
		if *output == "json" {
			return writeOutputJSON(out, response, *watch > 0)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tNODE\tSTATE\tHEALTH\tVCPU\tMEMORY")
		for _, service := range response.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d MB\n", service.Name, valueOrDash(service.Node), service.State, service.Health, service.VCPUs, service.MemoryMB)
		}
		return w.Flush()
	})
}

func runService(cfg cliConfig, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("service", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", "table", "table or json")
	watch := flags.Duration("watch", 0, "polling interval")
	if err := flags.Parse(reorderDetailArgs(args)); err != nil || flags.NArg() != 1 {
		return usageError("usage: fireworkctl service <service-name> [--output table|json] [--watch 5s]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if err := validateWatch(*watch); err != nil {
		return err
	}
	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}
	name := flags.Arg(0)
	return poll(out, *watch, *output == "table", func() error {
		var response controlplane.ServiceDetail
		if err := client.get(context.Background(), "/v1/services/"+url.PathEscape(name), &response); err != nil {
			return err
		}
		if *output == "json" {
			return writeOutputJSON(out, response, *watch > 0)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "SERVICE\t%s\nNODE\t%s\nDESIRED NODE\t%s\nACTUAL NODE\t%s\nSTATE\t%s\nHEALTH\t%s\nVCPU\t%d\nMEMORY\t%d MB\nIMAGE\t%s\nKERNEL\t%s\nPID\t%d\nRESTARTS\t%d\nSERVICE OBSERVED\t%s\nLAST TRANSITION\t%s\nREASON\t%s\nMESSAGE\t%s\nNETWORK ADDRESS\t%s\nROUTING HOSTNAME\t%s\n", response.Name, valueOrDash(response.Node), valueOrDash(response.DesiredNode), valueOrDash(response.ActualNode), response.State, response.Health, response.VCPUs, response.MemoryMB, valueOrDash(response.DesiredImage), valueOrDash(response.DesiredKernel), response.PID, response.RestartCount, formatTime(response.ServiceObservedAt), formatTime(response.LastTransitionAt), valueOrDash(response.ReasonCode), valueOrDash(response.Message), valueOrDash(response.NetworkAddress), valueOrDash(response.RoutingHostname))
		hc := response.HealthCheck
		fmt.Fprintf(w, "HEALTH CHECK\t%s\nHEALTH CHECK STATE\t%s\nHEALTH CHECKED\t%s\nHEALTH FAILURES\t%d\nHEALTH LAST ERROR\t%s\n", valueOrDash(hc.Type), valueOrUnknown(hc.State), formatTime(hc.LastCheckedAt), hc.Failures, valueOrDash(hc.LastError))
		fmt.Fprintf(w, "DESIRED REVISION\t%s\nPLACEMENT REVISION\t%s\nRENDERED REVISION\t%s\nAPPLIED REVISION\t%s\n", valueOrDash(response.DesiredRevision), valueOrDash(response.PlacementRevision), valueOrDash(response.RenderedRevision), valueOrDash(response.AppliedRevision))
		if len(response.PortForwards) > 0 {
			fmt.Fprintln(w, "\nHOST PORT\tVM PORT")
			for _, port := range response.PortForwards {
				fmt.Fprintf(w, "%d\t%d\n", port.HostPort, port.VMPort)
			}
		}
		if len(response.Volumes) > 0 {
			fmt.Fprintln(w, "\nVOLUME\tTYPE\tMOUNT PATH\tBOUND NODE\tBACKEND\tDESIRED BYTES\tAPPLIED BYTES\tGENERATION\tSTATE\tLAST ERROR")
			for _, volume := range response.Volumes {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
					volume.LogicalID, volume.Type, volume.MountPath, valueOrDash(volume.BoundNode),
					valueOrDash(volume.SharedBackendID), volume.DesiredSizeBytes, volume.AppliedSizeBytes,
					volume.ResizeGeneration, volume.State, valueOrDash(volume.LastError))
			}
		}
		return w.Flush()
	})
}

func (c *apiClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.endpoint, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &httpStatusError{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding API response: %w", err)
	}
	return nil
}

func newAPIClient(cfg cliConfig) (*apiClient, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("API endpoint is required (--endpoint or FIREWORK_API_ENDPOINT)")
	}
	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("API endpoint must be an https URL")
	}
	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("operator token file is required (--token-file or FIREWORK_API_TOKEN_FILE)")
	}
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading token file: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	return &apiClient{endpoint: strings.TrimRight(cfg.Endpoint, "/"), token: strings.TrimSpace(string(token)), http: &http.Client{Transport: transport, Timeout: 20 * time.Second}}, nil
}

func loadCLIConfig(path string) (cliConfig, error) {
	var cfg cliConfig
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config: %w", err)
	}
	return cfg, nil
}

func defaultConfigPath() string {
	if value := os.Getenv("FIREWORK_API_CONFIG"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "fireworkctl.yaml"
	}
	return filepath.Join(home, ".config", "firework", "config.yaml")
}

func configPathFromArgs(args []string) string {
	path := defaultConfigPath()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--config" {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--config=") {
			return strings.TrimPrefix(args[i], "--config=")
		}
	}
	return path
}

func applyEnv(cfg *cliConfig) {
	if value := os.Getenv("FIREWORK_API_ENDPOINT"); value != "" {
		cfg.Endpoint = value
	}
	if value := os.Getenv("FIREWORK_API_CA_FILE"); value != "" {
		cfg.CAFile = value
	}
	if value := os.Getenv("FIREWORK_API_TOKEN_FILE"); value != "" {
		cfg.TokenFile = value
	}
}

func poll(out io.Writer, interval time.Duration, clear bool, fn func() error) error {
	for {
		if err := fn(); err != nil {
			return err
		}
		if interval <= 0 {
			return nil
		}
		time.Sleep(interval)
		if clear {
			fmt.Fprint(out, "\033[H\033[2J")
		}
	}
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeOutputJSON(out io.Writer, value any, stream bool) error {
	if stream {
		return json.NewEncoder(out).Encode(value)
	}
	return writeJSON(out, value)
}

func validateOutput(output string) error {
	if output != "table" && output != "json" {
		return usageError("output must be table or json")
	}
	return nil
}

func validateWatch(interval time.Duration) error {
	if interval < 0 {
		return usageError("watch interval must not be negative")
	}
	return nil
}

func validateFilter(name, value string, allowed []string) error {
	if value == "" {
		return nil
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return usageError(fmt.Sprintf("%s must be one of: %s", name, strings.Join(allowed, ", ")))
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}

func formatAge(age int64, missing bool) string {
	if missing {
		return "unknown"
	}
	return fmt.Sprintf("%ds", age)
}

func formatBool(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  fireworkctl [global options] <command> [options]

Commands:
  nodes                 List deployment nodes
  node <node-id>        Show node details
  services              List deployment services
  service <name>        Show service details

Global options:
  --config <path>       Configuration file
  --endpoint <url>      Control-plane API endpoint
  --ca-file <path>      CA bundle for API TLS
  --token-file <path>   Operator token file
  --version             Print version
  -h, --help            Show this help
`)
}

func printCommandUsage(out io.Writer, command string) {
	usage := map[string]string{
		"nodes":    "Usage: fireworkctl nodes [--state ready|draining|down|stale|unknown] [--output table|json] [--watch 5s]\n",
		"node":     "Usage: fireworkctl node <node-id> [--output table|json] [--watch 5s]\n",
		"services": "Usage: fireworkctl services [--state pending|running|stopped|failed|unknown] [--health healthy|unhealthy|unknown|not_configured] [--node NODE] [--output table|json] [--watch 5s]\n",
		"service":  "Usage: fireworkctl service <service-name> [--output table|json] [--watch 5s]\n",
	}
	if text, ok := usage[command]; ok {
		fmt.Fprint(out, text)
		return
	}
	printUsage(out)
}

func globalHelpRequested(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-h" || arg == "--help" {
			return true
		}
		if isSubcommand(arg) {
			return false
		}
		if isGlobalFlagWithValue(arg) && !strings.Contains(arg, "=") {
			i++
		}
	}
	return false
}

func subcommandHelp(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if isGlobalFlagWithValue(arg) && !strings.Contains(arg, "=") {
			i++
			continue
		}
		if !isSubcommand(arg) {
			continue
		}
		for _, next := range args[i+1:] {
			if next == "-h" || next == "--help" {
				return arg, true
			}
		}
		return "", false
	}
	return "", false
}

func isSubcommand(arg string) bool {
	switch arg {
	case "nodes", "node", "services", "service":
		return true
	default:
		return false
	}
}

func isGlobalFlagWithValue(arg string) bool {
	if strings.Contains(arg, "=") {
		arg = strings.SplitN(arg, "=", 2)[0]
	}
	switch arg {
	case "--config", "--endpoint", "--ca-file", "--token-file":
		return true
	default:
		return false
	}
}

func versionRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--version" {
			return true
		}
	}
	return false
}

func usageError(message string) error { return fmt.Errorf("%s", message) }

func reorderDetailArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--output" || arg == "--watch" {
			flags = append(flags, arg)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if strings.HasPrefix(arg, "--output=") || strings.HasPrefix(arg, "--watch=") {
			flags = append(flags, arg)
			continue
		}
		positional = append(positional, arg)
	}
	return append(flags, positional...)
}
