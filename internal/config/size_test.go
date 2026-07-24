package config

import "testing"

func TestParseVolumeSize(t *testing.T) {
	tests := []struct {
		value string
		want  int64
		ok    bool
	}{
		{"1Mi", MiB, true}, {"10Gi", 10 * GiB, true}, {"0Gi", 0, false},
		{"1Ti", 0, false}, {"1.5Gi", 0, false}, {"-1Gi", 0, false}, {"10GB", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := ParseVolumeSize(tt.value)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseVolumeSize(%q) error = %v", tt.value, err)
			}
			if err == nil && got != tt.want {
				t.Fatalf("ParseVolumeSize(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseStorageCapacityAllowsTi(t *testing.T) {
	got, err := ParseStorageCapacity("2Ti")
	if err != nil || got != 2*TiB {
		t.Fatalf("ParseStorageCapacity = %d, %v", got, err)
	}
}
