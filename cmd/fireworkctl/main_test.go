package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunWithoutCommandPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err != nil {
		t.Fatalf("run returned an error: %v", err)
	}

	for _, want := range []string{
		"Usage:",
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
