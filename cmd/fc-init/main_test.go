//go:build linux

package main

import (
	"reflect"
	"testing"
)

func TestResolveUserSpec_NumericUIDDefaultsGID(t *testing.T) {
	uid, gid, username, home, err := resolveUserSpec("1001")
	if err != nil {
		t.Fatalf("resolveUserSpec returned error: %v", err)
	}
	if uid != 1001 || gid != 1001 {
		t.Fatalf("expected uid/gid 1001/1001, got %d/%d", uid, gid)
	}
	if username != "" || home != "" {
		t.Fatalf("expected empty username/home for numeric uid, got %q/%q", username, home)
	}
}

func TestResolveUserSpec_NumericUIDAndGID(t *testing.T) {
	uid, gid, _, _, err := resolveUserSpec("1001:2002")
	if err != nil {
		t.Fatalf("resolveUserSpec returned error: %v", err)
	}
	if uid != 1001 || gid != 2002 {
		t.Fatalf("expected uid/gid 1001/2002, got %d/%d", uid, gid)
	}
}

func TestResolveUserSpec_InvalidSpec(t *testing.T) {
	if _, _, _, _, err := resolveUserSpec(":1000"); err == nil {
		t.Fatal("expected error for invalid user spec")
	}
}

func TestNormalizeWritablePaths(t *testing.T) {
	got := normalizeWritablePaths([]string{
		"",
		"relative/path",
		"/tmp",
		"/tmp/",
		"/var/lib/../lib/firework",
		"/tmp",
	})
	want := []string{"/tmp", "/var/lib/firework"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestParseFireworkEnvArg_LegacyRaw(t *testing.T) {
	key, val, ok, err := parseFireworkEnvArg("firework.env.DATABASE_URL=postgres://user:pass@host/db")
	if err != nil {
		t.Fatalf("parseFireworkEnvArg returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected env arg to be parsed")
	}
	if key != "DATABASE_URL" || val != "postgres://user:pass@host/db" {
		t.Fatalf("expected DATABASE_URL raw value, got %s=%s", key, val)
	}
}

func TestParseFireworkEnvArg_Base64WhitespaceValue(t *testing.T) {
	key, val, ok, err := parseFireworkEnvArg("firework.env64.MESSAGE=aGVsbG8gd29ybGQ")
	if err != nil {
		t.Fatalf("parseFireworkEnvArg returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected encoded env arg to be parsed")
	}
	if key != "MESSAGE" || val != "hello world" {
		t.Fatalf("expected MESSAGE=hello world, got %s=%s", key, val)
	}
}

func TestParseFireworkEnvArg_IgnoresNonEnvArg(t *testing.T) {
	_, _, ok, err := parseFireworkEnvArg("console=ttyS0")
	if err != nil {
		t.Fatalf("parseFireworkEnvArg returned error: %v", err)
	}
	if ok {
		t.Fatal("expected non-env arg to be ignored")
	}
}
