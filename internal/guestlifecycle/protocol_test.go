package guestlifecycle

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	want := Request{Version: Version, Operation: QuiesceOperation, TimeoutMS: 12000}
	var framed bytes.Buffer
	if err := WriteRequest(&framed, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&framed)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("request round trip got %#v, want %#v", got, want)
	}
}

func TestResponseRoundTripPreservesPerMountFailures(t *testing.T) {
	want := Response{
		Version: Version, Operation: QuiesceOperation, AppExited: true,
		AppExitCode: 143, SyncDone: true,
		Mounts: []MountResult{
			{Name: "data", Path: "/var/lib/app", Unmounted: true},
			{Name: "cache", Path: "/var/cache/app", Error: "device busy"},
		},
	}
	var framed bytes.Buffer
	if err := WriteResponse(&framed, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResponse(&framed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mounts[0] != want.Mounts[0] || got.Mounts[1] != want.Mounts[1] {
		t.Fatalf("mount results changed in round trip: %#v", got.Mounts)
	}
	if got.Successful() {
		t.Fatal("response with a failed mount must not report quiesce success")
	}
}

func TestResponseSuccessDoesNotRequireZeroApplicationExit(t *testing.T) {
	response := Response{
		Version: Version, Operation: QuiesceOperation, AppExited: true,
		AppExitCode: 1, SyncDone: true,
		Mounts: []MountResult{{Name: "data", Path: "/data", Unmounted: true}},
	}
	if !response.Successful() {
		t.Fatal("a clean quiesce must accept an application that exited nonzero")
	}
}

func TestReadRequestRejectsMalformedAndOversizedFrames(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "unknown field", data: frameJSON(`{"version":1,"operation":"quiesce","timeout_ms":1000,"extra":true}`)},
		{name: "unsupported version", data: frameJSON(`{"version":2,"operation":"quiesce","timeout_ms":1000}`)},
		{name: "unsupported operation", data: frameJSON(`{"version":1,"operation":"terminate","timeout_ms":1000}`)},
		{name: "missing timeout", data: frameJSON(`{"version":1,"operation":"quiesce"}`)},
		{name: "trailing value", data: frameJSON(`{"version":1,"operation":"quiesce","timeout_ms":1000}{}`)},
		{name: "truncated body", data: append([]byte{0, 0, 0, 8}, []byte(`{}`)...)},
		{name: "oversized", data: frameLength(MaxFrameBytes + 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ReadRequest(bytes.NewReader(test.data)); err == nil {
				t.Fatal("expected malformed lifecycle input to be rejected")
			}
		})
	}
}

func TestResponseRejectsUnboundedAndDuplicateMountResults(t *testing.T) {
	for _, test := range []struct {
		name     string
		response Response
	}{
		{name: "duplicate names", response: Response{Version: Version, Operation: QuiesceOperation, Mounts: []MountResult{{Name: "data", Path: "/data", Error: "busy"}, {Name: "data", Path: "/other", Error: "busy"}}}},
		{name: "oversized error", response: Response{Version: Version, Operation: QuiesceOperation, Mounts: []MountResult{{Name: "data", Path: "/data", Error: strings.Repeat("x", MaxErrorBytes+1)}}}},
		{name: "too many mounts", response: Response{Version: Version, Operation: QuiesceOperation, Mounts: make([]MountResult, MaxMountResults+1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.response.Validate(); err == nil {
				t.Fatal("expected invalid lifecycle response to be rejected")
			}
		})
	}
}

func frameJSON(data string) []byte {
	var out bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	out.Write(header[:])
	out.WriteString(data)
	return out.Bytes()
}

func frameLength(size int) []byte {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(size))
	return header[:]
}

func TestFrameReaderPropagatesEOF(t *testing.T) {
	if _, err := ReadRequest(bytes.NewReader(nil)); err == nil || err == io.EOF {
		t.Fatalf("expected contextual EOF error, got %v", err)
	}
}
