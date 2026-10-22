package config

import (
	"fmt"
	"regexp"
	"sort"
)

// MaxNodeLabelLen bounds a scheduling label to one DNS label.
const MaxNodeLabelLen = 63

// nodeLabelPattern is the canonical node-label syntax shared by workload
// node_type and agent node_labels. Values are never coerced: lower-casing a
// node_type would silently rename the direct-mode nodes/<node_type>.yaml key.
var nodeLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidateNodeLabel reports whether value is a canonical scheduling label:
// 1-63 lower-case letters, digits, or internal hyphens, starting and ending
// with a letter or digit.
func ValidateNodeLabel(value string) error {
	if !nodeLabelPattern.MatchString(value) {
		return fmt.Errorf("%q must be 1-%d lower-case letters, digits, or internal '-' characters", value, MaxNodeLabelLen)
	}
	return nil
}

// NormalizeNodeLabels returns a sorted, de-duplicated copy of labels. It does
// not validate them; empty values are dropped.
func NormalizeNodeLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
