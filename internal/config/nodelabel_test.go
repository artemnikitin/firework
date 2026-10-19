package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateNodeLabel(t *testing.T) {
	tests := []struct {
		value string
		valid bool
	}{
		{"compute", true},
		{"general-purpose", true},
		{"a", true},
		{"arm64", true},
		{strings.Repeat("a", 63), true},
		{"", false},
		{strings.Repeat("a", 64), false},
		{"Compute", false},
		{"general_purpose", false},
		{"-compute", false},
		{"compute-", false},
		{"web.backend", false},
		{" compute", false},
	}
	for _, tt := range tests {
		err := ValidateNodeLabel(tt.value)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateNodeLabel(%q) error = %v, want valid=%v", tt.value, err, tt.valid)
		}
	}
}

func TestNormalizeNodeLabels(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"only empty", []string{""}, nil},
		{"sorted and deduplicated", []string{"web", "backend", "web", ""}, []string{"backend", "web"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeNodeLabels(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NormalizeNodeLabels(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeNodeLabels_DoesNotAliasInput(t *testing.T) {
	in := []string{"web", "backend"}
	_ = NormalizeNodeLabels(in)
	if in[0] != "web" || in[1] != "backend" {
		t.Fatalf("input mutated: %v", in)
	}
}
