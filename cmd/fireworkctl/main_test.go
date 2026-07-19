package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunWithoutCommandPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}

	for _, want := range []string{
		"Usage:",
		"status                Show current revision convergence",
		"nodes                 List deployment nodes",
		"node <node-id>        Show node details",
		"services              List deployment services",
		"service <name>        Show service details",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestRunHelpPrintsUsage(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			if err := run([]string{arg}, &out); err != nil {
				t.Fatalf("run returned an error: %v", err)
			}
			if !strings.Contains(out.String(), "Commands:") {
				t.Fatalf("help output does not list commands:\n%s", out.String())
			}
		})
	}
}

func TestRunSubcommandHelpDoesNotRequireConfiguration(t *testing.T) {
	for _, command := range []string{"status", "nodes", "node", "services", "service"} {
		t.Run(command, func(t *testing.T) {
			var out bytes.Buffer
			if err := run([]string{"--endpoint", "https://example.com", command, "--help"}, &out); err != nil {
				t.Fatalf("run returned an error: %v", err)
			}
			if !strings.Contains(out.String(), "Usage: fireworkctl "+command) {
				t.Fatalf("unexpected help output: %s", out.String())
			}
		})
	}
}

func TestRunVersionAcceptsOtherGlobalOptions(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--endpoint", "https://example.com", "--version"}, &out); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}
	if !strings.HasPrefix(out.String(), "fireworkctl ") {
		t.Fatalf("unexpected version output: %s", out.String())
	}
}

func TestConfigPathFromArgsSupportsEqualsSyntax(t *testing.T) {
	if got := configPathFromArgs([]string{"--config=/tmp/firework.yaml", "nodes"}); got != "/tmp/firework.yaml" {
		t.Fatalf("config path = %q, want /tmp/firework.yaml", got)
	}
}

func TestCommandValidationRejectsInvalidValuesBeforeClientSetup(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "node state", args: []string{"nodes", "--state", "raedy"}, want: "state must be one of"},
		{name: "service health", args: []string{"services", "--health", "heathy"}, want: "health must be one of"},
		{name: "output", args: []string{"nodes", "--output", "yaml"}, want: "output must be table or json"},
		{name: "negative watch", args: []string{"nodes", "--watch", "-1s"}, want: "watch interval must not be negative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(test.args, &out)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestJSONWatchOutputIsCompactAndHasNoTerminalControlSequence(t *testing.T) {
	var out bytes.Buffer
	value := map[string]any{"count": 1}
	if err := writeOutputJSON(&out, value, true); err != nil {
		t.Fatal(err)
	}
	if err := writeOutputJSON(&out, value, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\033") || strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("watch JSON output is not newline-delimited: %q", out.String())
	}
	decoder := json.NewDecoder(&out)
	for i := 0; i < 2; i++ {
		var decoded map[string]any
		if err := decoder.Decode(&decoded); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
	}
}

func TestPollCanAvoidTerminalClearing(t *testing.T) {
	var out bytes.Buffer
	seen := 0
	wantErr := errors.New("stop")
	err := poll(&out, time.Nanosecond, false, func() error {
		seen++
		if seen == 2 {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("poll error = %v, want %v", err, wantErr)
	}
	if strings.Contains(out.String(), "\033") {
		t.Fatalf("poll emitted terminal control sequence: %q", out.String())
	}
}
