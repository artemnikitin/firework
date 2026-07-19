package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	MiB int64 = 1024 * 1024
	GiB int64 = 1024 * MiB
	TiB int64 = 1024 * GiB
)

// ParseVolumeSize accepts the deliberately small application quota grammar:
// a positive integer followed by Mi or Gi.
func ParseVolumeSize(value string) (int64, error) {
	return parseSize(value, false)
}

// ParseStorageCapacity accepts Mi, Gi, and Ti for operator-owned pool budgets.
func ParseStorageCapacity(value string) (int64, error) {
	return parseSize(value, true)
}

func parseSize(value string, allowTi bool) (int64, error) {
	value = strings.TrimSpace(value)
	units := "Mi or Gi"
	if allowTi {
		units = "Mi, Gi, or Ti"
	}
	var multiplier int64
	switch {
	case strings.HasSuffix(value, "Mi"):
		multiplier = MiB
		value = strings.TrimSuffix(value, "Mi")
	case strings.HasSuffix(value, "Gi"):
		multiplier = GiB
		value = strings.TrimSuffix(value, "Gi")
	case allowTi && strings.HasSuffix(value, "Ti"):
		multiplier = TiB
		value = strings.TrimSuffix(value, "Ti")
	default:
		return 0, fmt.Errorf("must be a positive integer followed by %s", units)
	}

	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("must be a positive integer followed by %s", units)
	}
	if n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("size overflows int64 bytes")
	}
	return n * multiplier, nil
}
