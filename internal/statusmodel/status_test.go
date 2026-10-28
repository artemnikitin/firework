package statusmodel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBoundedMessageSanitizesURLsAndControls(t *testing.T) {
	message := "download\nhttps://user:secret@example.com/image?token=secret#fragment failed " + strings.Repeat("x", 400)
	got := BoundedMessage(message)
	if strings.Contains(got, "secret") || strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("message was not sanitized: %q", got)
	}
	if len([]rune(got)) != MaxMessageLen {
		t.Fatalf("bounded length = %d, want %d", len([]rune(got)), MaxMessageLen)
	}
}

// The control plane refuses to call a node converged unless every blocking
// condition is present, and the agent is what supplies them. If the two lists
// ever diverge, no node can be classified converged again — silently, and
// permanently. Keeping both derived from this package makes that structural,
// and this pins it.
func TestBlockingConditionsAreASubsetOfReported(t *testing.T) {
	reported := make(map[string]struct{})
	for _, conditionType := range ReconciliationConditionTypes() {
		reported[conditionType] = struct{}{}
	}
	for _, required := range BlockingConditionTypes() {
		if _, ok := reported[required]; !ok {
			t.Fatalf("blocking condition %q is required but never reported by an agent", required)
		}
		if !IsBlockingCondition(required) {
			t.Fatalf("%q is in the blocking list but IsBlockingCondition says otherwise", required)
		}
	}
	for _, conditionType := range ReconciliationConditionTypes() {
		if !IsBlockingCondition(conditionType) && !IsNonBlockingCondition(conditionType) {
			t.Fatalf("reported condition %q is classified as neither blocking nor degrading", conditionType)
		}
	}
}

// The exported accessors must not hand out the package's own slices.
func TestConditionAccessorsReturnCopies(t *testing.T) {
	first := BlockingConditionTypes()
	first[0] = "mutated"
	if BlockingConditionTypes()[0] == "mutated" {
		t.Fatal("BlockingConditionTypes exposed its backing array")
	}
}

func TestActualOnlyWireShapeDistinguishesLegacyOmissionFromV2Empty(t *testing.T) {
	legacy, err := json.Marshal(AgentStatus{SchemaVersion: LegacySchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "actual_only_services") {
		t.Fatalf("legacy v1 wire shape unexpectedly includes v2 inventory: %s", legacy)
	}
	v2, err := json.Marshal(AgentStatus{SchemaVersion: SchemaVersion, ActualOnlyServices: []ServiceStatus{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(v2), `"actual_only_services":[]`) {
		t.Fatalf("v2 empty inventory was omitted: %s", v2)
	}
}
