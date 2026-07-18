// Package statusmodel defines the bounded, versioned status exchanged by
// agents and the control plane. Keep this package provider-neutral and free of
// runtime dependencies so mixed-version agents can be decoded safely.
package statusmodel

import (
	"net/url"
	"strings"
	"time"
	"unicode"
)

const (
	SchemaVersion = 1
	MaxMessageLen = 256
)

type Phase string

const (
	PhaseUnknown     Phase = "unknown"
	PhaseReconciling Phase = "reconciling"
	PhaseReady       Phase = "ready"
	PhaseFailed      Phase = "failed"
)

type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "true"
	ConditionFalse   ConditionStatus = "false"
	ConditionUnknown ConditionStatus = "unknown"
)

type Condition struct {
	Type             string          `json:"type"`
	Status           ConditionStatus `json:"status"`
	ReasonCode       string          `json:"reason_code,omitempty"`
	Message          string          `json:"message,omitempty"`
	LastTransitionAt time.Time       `json:"last_transition_at,omitempty"`
}

type ServiceStatus struct {
	Name                string    `json:"name"`
	VMState             string    `json:"vm_state"`
	PID                 int       `json:"pid,omitempty"`
	NetworkAddress      string    `json:"network_address,omitempty"`
	Health              string    `json:"health"`
	HealthCheckType     string    `json:"health_check_type,omitempty"`
	HealthLastCheckedAt time.Time `json:"health_last_checked_at,omitempty"`
	HealthFailures      int       `json:"health_failures,omitempty"`
	RestartCount        int       `json:"restart_count,omitempty"`
	LastTransitionAt    time.Time `json:"last_transition_at,omitempty"`
	ReasonCode          string    `json:"reason_code,omitempty"`
	Message             string    `json:"message,omitempty"`
}

type AgentStatus struct {
	SchemaVersion     int             `json:"schema_version"`
	AgentVersion      string          `json:"agent_version,omitempty"`
	NodeID            string          `json:"node"`
	ObservedAt        time.Time       `json:"observed_at"`
	Phase             Phase           `json:"phase"`
	DesiredRevision   string          `json:"desired_revision,omitempty"`
	PlacementRevision string          `json:"placement_revision,omitempty"`
	ObservedRevision  string          `json:"observed_revision,omitempty"`
	AppliedRevision   string          `json:"applied_revision,omitempty"`
	LastAppliedAt     time.Time       `json:"last_applied_at,omitempty"`
	DesiredServices   int             `json:"desired_services"`
	ReadyServices     int             `json:"ready_services"`
	ReasonCode        string          `json:"reason_code,omitempty"`
	Message           string          `json:"message,omitempty"`
	Conditions        []Condition     `json:"conditions,omitempty"`
	Services          []ServiceStatus `json:"services,omitempty"`
}

func BoundedMessage(message string) string {
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	parts := strings.Fields(message)
	for i, part := range parts {
		parts[i] = sanitizeURL(part)
	}
	message = strings.Join(parts, " ")
	runes := []rune(message)
	if len(runes) <= MaxMessageLen {
		return message
	}
	return string(runes[:MaxMessageLen])
}

func sanitizeURL(value string) string {
	if !strings.Contains(value, "://") {
		return value
	}
	prefix := ""
	suffix := ""
	for len(value) > 0 && strings.ContainsRune("([{<\"'", rune(value[0])) {
		prefix += value[:1]
		value = value[1:]
	}
	for len(value) > 0 && strings.ContainsRune(")]}>\"',.;", rune(value[len(value)-1])) {
		suffix = value[len(value)-1:] + suffix
		value = value[:len(value)-1]
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return prefix + strings.SplitN(strings.SplitN(value, "?", 2)[0], "#", 2)[0] + suffix
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return prefix + parsed.String() + suffix
}
