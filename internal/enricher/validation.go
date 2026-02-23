package enricher

import (
	"fmt"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
)

// ValidationError holds multiple validation issues.
type ValidationError struct {
	Errors []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed with %d error(s):\n  - %s",
		len(e.Errors), strings.Join(e.Errors, "\n  - "))
}

func (e *ValidationError) add(msg string) {
	e.Errors = append(e.Errors, msg)
}

func (e *ValidationError) addf(format string, args ...any) {
	e.Errors = append(e.Errors, fmt.Sprintf(format, args...))
}

func (e *ValidationError) hasErrors() bool {
	return len(e.Errors) > 0
}

// Warn represents a non-fatal issue found during validation.
type Warn struct {
	Message string
}

// ValidateInput checks the raw user config for errors.
func ValidateInput(input *InputConfig) error {
	ve := &ValidationError{}

	svcNames := make(map[string]bool)
	for _, s := range input.Services {
		if s.Name == "" {
			ve.add("service with empty name")
			continue
		}
		if svcNames[s.Name] {
			ve.addf("duplicate service name: %s", s.Name)
		}
		svcNames[s.Name] = true

		if s.Image == "" {
			ve.addf("service %s: missing image", s.Name)
		}

		if s.NodeType == "" {
			ve.addf("service %s: missing node_type", s.Name)
		}

		if s.HealthCheck != nil {
			if s.HealthCheck.Type != "http" && s.HealthCheck.Type != "tcp" {
				ve.addf("service %s: invalid health check type %q (must be http or tcp)", s.Name, s.HealthCheck.Type)
			}
		}
	}

	if ve.hasErrors() {
		return ve
	}
	return nil
}

// ValidateOutput checks a fully enriched NodeConfig for correctness.
func ValidateOutput(nc config.NodeConfig) error {
	ve := &ValidationError{}

	if nc.Node == "" {
		ve.add("node config: empty node name")
	}

	svcNames := make(map[string]bool)
	for _, svc := range nc.Services {
		if svc.Name == "" {
			ve.add("service with empty name")
			continue
		}
		if svcNames[svc.Name] {
			ve.addf("duplicate service name in output: %s", svc.Name)
		}
		svcNames[svc.Name] = true

		if svc.Image == "" {
			ve.addf("service %s: missing image", svc.Name)
		}
		if svc.Kernel == "" {
			ve.addf("service %s: missing kernel", svc.Name)
		}
		if svc.VCPUs == 0 {
			ve.addf("service %s: zero vcpus", svc.Name)
		}
		if svc.MemoryMB == 0 {
			ve.addf("service %s: zero memory", svc.Name)
		}
	}

	if ve.hasErrors() {
		return ve
	}
	return nil
}

// CheckWarnings finds non-fatal issues in the input.
func CheckWarnings(input *InputConfig) []Warn {
	var warns []Warn

	for _, svc := range input.Services {
		if svc.HealthCheck != nil && !svc.Network {
			warns = append(warns, Warn{
				Message: fmt.Sprintf("service %s has health check but network is disabled", svc.Name),
			})
		}
	}

	return warns
}
