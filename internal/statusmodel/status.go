// Package statusmodel defines the bounded, versioned status exchanged by
// agents and the control plane. Keep this package provider-neutral and free of
// runtime dependencies so mixed-version agents can be decoded safely.
package statusmodel

import (
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaVersion       = 1
	MaxMessageLen       = 256
	MaxConditions       = 16
	MaxServices         = 256
	MaxServiceNameLen   = 128
	MaxConditionTypeLen = 64
	MaxReasonCodeLen    = 64
	MaxRevisionLen      = 256

	// The following bound every remaining AgentStatus/ServiceStatus/
	// VolumeStatus string field that would otherwise serialize unbounded.
	// registry.go's maxRegistryRequestBytes is only a real cap on a heartbeat
	// if every field that scales with MaxServices or config.MaxServiceVolumes
	// (6400 volumes at the product of the two) is bounded here; see
	// TestMaxRegistryRequestBytesExceedsLargestValidHeartbeat.
	// AgentVersion is `git describe --tags --always --dirty` at build time
	// (see the root Makefile's VERSION), not a fixed-format value, and it
	// appears once per heartbeat rather than scaling with MaxServices or
	// config.MaxServiceVolumes — so unlike the fields below it contributes
	// nothing to the 6400x budget this bounding pass exists for. The bound
	// here is generous headroom against a pathological tag name, not a tight
	// fit: rejecting it outright drops the agent's entire status (see
	// handleHeartbeat's validate-or-drop fallback), which is worse than the
	// field being slightly larger than expected.
	MaxAgentVersionLen   = 256
	MaxEnumLen           = 32 // VMState, Health, HealthCheckType, VolumeStatus.Type, VolumeStatus.State
	MaxNetworkAddressLen = 64
	// BoundNode and SharedBackendID are exact-match identity keys. Reject
	// overlong values at input boundaries because truncation changes identity.
	MaxVolumeIDLen = 128
	// MaxLogicalIDLen must cover visibility.go's composed
	// "service.Name + "/" + volume.Name": up to MaxServiceNameLen (128) plus
	// "/" plus a volume name, which the enricher caps at 63 characters
	// (validation.go's volumeNamePattern) = 192. A tighter bound would reject
	// a legitimately-named service/volume pair outright, dropping that node's
	// entire agent_status (see handleHeartbeat's validate-or-drop fallback).
	MaxLogicalIDLen = 192
	MaxMountPathLen = 256
)

// blockingConditionTypes are the reconciliation stages whose failure makes a
// node failed rather than degraded. nonBlockingConditionTypes only degrade it.
//
// Both sides of the contract read these: the agent finalizes every one of them
// on each tick, and the control plane requires the blocking set to be present
// before it will call a node converged. They live here so a rename cannot land
// on one side only — that would leave no node ever classifiable as converged,
// silently and permanently.
var (
	blockingConditionTypes = []string{
		"ConfigFetched", "ConfigParsed", "NetworkReady", "CapacityReady",
		"ImagesReady", "VMsReconciled", "Reconciled", "LocalRoutesReady",
	}
	// VolumeSizesApplied is false while this node is running a volume at a
	// size other than the one the desired revision asked for. It is
	// non-blocking because the workload is healthy — it is running, just not
	// at the requested quota — but it must not read as ordinary convergence,
	// or the operator sees a service quietly running at the wrong size with no
	// explanation.
	nonBlockingConditionTypes = []string{"PeerRoutesReady", "VolumeSizesApplied"}
)

// BlockingConditionTypes returns the conditions whose failure is fatal.
func BlockingConditionTypes() []string {
	return append([]string(nil), blockingConditionTypes...)
}

// ReconciliationConditionTypes returns every condition an agent reports for a
// tick, blocking and non-blocking alike.
func ReconciliationConditionTypes() []string {
	out := make([]string, 0, len(blockingConditionTypes)+len(nonBlockingConditionTypes))
	out = append(out, blockingConditionTypes...)
	return append(out, nonBlockingConditionTypes...)
}

// IsBlockingCondition reports whether a false condition of this type means the
// node has failed rather than merely degraded.
func IsBlockingCondition(conditionType string) bool {
	for _, known := range blockingConditionTypes {
		if known == conditionType {
			return true
		}
	}
	return false
}

// IsNonBlockingCondition reports whether this type is known and degrading.
func IsNonBlockingCondition(conditionType string) bool {
	for _, known := range nonBlockingConditionTypes {
		if known == conditionType {
			return true
		}
	}
	return false
}

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
	Name                string         `json:"name"`
	VMState             string         `json:"vm_state"`
	PID                 int            `json:"pid,omitempty"`
	NetworkAddress      string         `json:"network_address,omitempty"`
	Health              string         `json:"health"`
	HealthCheckType     string         `json:"health_check_type,omitempty"`
	HealthLastCheckedAt time.Time      `json:"health_last_checked_at,omitempty"`
	HealthFailures      int            `json:"health_failures,omitempty"`
	RestartCount        int            `json:"restart_count,omitempty"`
	LastTransitionAt    time.Time      `json:"last_transition_at,omitempty"`
	ReasonCode          string         `json:"reason_code,omitempty"`
	Message             string         `json:"message,omitempty"`
	Volumes             []VolumeStatus `json:"volumes,omitempty"`
}

type VolumeStatus struct {
	LogicalID        string `json:"logical_id"`
	Type             string `json:"type"`
	MountPath        string `json:"mount_path"`
	BoundNode        string `json:"bound_node,omitempty"`
	SharedBackendID  string `json:"shared_backend_id,omitempty"`
	DesiredSizeBytes int64  `json:"desired_size_bytes"`
	AppliedSizeBytes int64  `json:"applied_size_bytes,omitempty"`
	ResizeGeneration int64  `json:"resize_generation,omitempty"`
	State            string `json:"state"`
	LastError        string `json:"last_error,omitempty"`
	// RequestedSizeBytes is what the desired revision asked for, when that
	// differs from the effective DesiredSizeBytes the cluster accepted and
	// rendered. Equal sizes are reported only through DesiredSizeBytes, so an
	// unrejected volume's surface is unchanged.
	RequestedSizeBytes int64  `json:"requested_size_bytes,omitempty"`
	Rejected           bool   `json:"rejected,omitempty"`
	RejectedReason     string `json:"rejected_reason,omitempty"`
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
	ServicesTruncated bool            `json:"services_truncated,omitempty"`
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

// BoundedPath truncates a path-like field to MaxMountPathLen bytes. Unlike
// BoundedMessage it does not collapse whitespace or sanitize URLs — a path is
// not free text — and the bound is a byte count, matching
// validateVolumeStatus's check on the receiving side.
//
// This exists so a mount_path is bounded on the sender before it is ever
// marshaled, not only rejected on arrival: the enricher rejects an overlong
// mount_path at config-validation time (internal/enricher/validation.go), but
// a config applied without going through the enricher would otherwise
// produce a heartbeat that fails validateVolumeStatus outright, dropping the
// node's entire agent_status rather than just this one field.
func BoundedPath(path string) string {
	return truncateUTF8(path, MaxMountPathLen)
}

// BoundedLogicalID truncates a volume's composed "service/volume" identifier
// to MaxLogicalIDLen bytes, the same sender-side defense as BoundedPath but
// for LogicalID: the enricher's service-name (MaxServiceNameLen) and
// volume-name (63-character regex) limits sum to exactly MaxLogicalIDLen with
// no margin, so a config applied without going through the enricher could
// otherwise produce a LogicalID validateVolumeStatus rejects outright.
func BoundedLogicalID(logicalID string) string {
	return truncateUTF8(logicalID, MaxLogicalIDLen)
}

// truncateUTF8 truncates s to at most max bytes, trimming back further if
// needed so the result is never a truncated multi-byte UTF-8 sequence.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	truncated := s[:max]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
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
