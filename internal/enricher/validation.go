package enricher

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/artemnikitin/firework/internal/config"
	"github.com/artemnikitin/firework/internal/ingress"
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

// Warning codes are stable identifiers so callers can act on a specific
// warning class without matching free-text messages.
const (
	// WarnHealthCheckWithoutNetwork: a service defines a health check but has
	// networking disabled.
	WarnHealthCheckWithoutNetwork = "health_check_without_network"
	// WarnRemoteRoutingNoHostPort: a routed service has no usable first
	// port_forwards host port, so it cannot participate in remote multi-node
	// routing (remote nodes proxy through the host port).
	WarnRemoteRoutingNoHostPort = "remote_routing_no_host_port"
)

// Warn represents a non-fatal issue found during validation.
type Warn struct {
	// Code is a stable identifier for the warning class.
	Code    string
	Message string
}

// ValidateInput checks the raw user config for errors.
func ValidateInput(input *InputConfig) error {
	ve := &ValidationError{}
	validateVolumeDefaults(ve, input.Defaults.VolumeDefaults)

	svcNames := make(map[string]bool)
	subSeen := make(map[string]string)  // subdomain -> first service using it
	hostSeen := make(map[string]string) // normalized host -> first service using it
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

		validateRouting(ve, s, input.Defaults, subSeen, hostSeen)
		validateVolumes(ve, s)
	}

	if ve.hasErrors() {
		return ve
	}
	return nil
}

var volumeNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

func validateVolumeDefaults(ve *ValidationError, defs VolumeDefaults) {
	for field, value := range map[string]string{
		"volume_defaults.local_size":  defs.LocalSize,
		"volume_defaults.shared_size": defs.SharedSize,
	} {
		if value == "" {
			continue
		}
		if _, err := config.ParseVolumeSize(value); err != nil {
			ve.addf("%s: %v", field, err)
		}
	}
}

func validateVolumes(ve *ValidationError, svc ServiceSpec) {
	if len(svc.Volumes) > config.MaxServiceVolumes {
		ve.addf("service %s: at most %d volumes are supported", svc.Name, config.MaxServiceVolumes)
	}
	names := make(map[string]struct{}, len(svc.Volumes))
	paths := make(map[string]string, len(svc.Volumes))
	for _, volume := range svc.Volumes {
		prefix := fmt.Sprintf("service %s volume %s", svc.Name, volume.Name)
		if !volumeNamePattern.MatchString(volume.Name) {
			ve.addf("%s: name must be a DNS-label-like value of at most 63 characters", prefix)
		}
		if _, exists := names[volume.Name]; exists {
			ve.addf("service %s: duplicate volume name %q", svc.Name, volume.Name)
		}
		names[volume.Name] = struct{}{}

		if volume.Type != config.VolumeTypeLocal && volume.Type != config.VolumeTypeShared {
			ve.addf("%s: type must be local or shared", prefix)
		}
		if volume.BoundNode != "" || volume.SharedBackendID != "" || volume.SizeBytes != 0 || volume.ResizeGeneration != 0 {
			ve.addf("%s: bound_node, shared_backend_id, size_bytes, and resize_generation are system-owned", prefix)
		}

		cleaned := filepath.Clean(volume.MountPath)
		if volume.MountPath == "" || !filepath.IsAbs(volume.MountPath) || cleaned != volume.MountPath {
			ve.addf("%s: mount_path must be a clean absolute path", prefix)
		} else if forbiddenVolumeMount(cleaned) {
			ve.addf("%s: mount_path %q is reserved", prefix, cleaned)
		}
		if other, exists := paths[cleaned]; exists {
			ve.addf("service %s volumes %s and %s: duplicate mount_path %q", svc.Name, other, volume.Name, cleaned)
		}
		for existingPath, existingName := range paths {
			if pathContains(existingPath, cleaned) || pathContains(cleaned, existingPath) {
				ve.addf("service %s volumes %s and %s: overlapping mount paths %q and %q", svc.Name, existingName, volume.Name, existingPath, cleaned)
			}
		}
		paths[cleaned] = volume.Name

		if volume.Size != "" {
			if _, err := config.ParseVolumeSize(volume.Size); err != nil {
				ve.addf("%s size: %v", prefix, err)
			}
		}
	}
}

func forbiddenVolumeMount(path string) bool {
	for _, reserved := range []string{"/", "/proc", "/sys", "/dev", "/run", "/tmp"} {
		if path == reserved || pathContains(reserved, path) {
			return true
		}
	}
	return false
}

func pathContains(parent, child string) bool {
	return parent != child && strings.HasPrefix(child, parent+string(filepath.Separator))
}

// validateRouting checks a service's public-routing metadata: at most one of
// metadata.subdomain / metadata.host, a valid value for whichever is set, no
// duplicate subdomains or exact hosts across services, and a usable network and
// backend port for any routed service.
func validateRouting(ve *ValidationError, s ServiceSpec, defs Defaults, subSeen, hostSeen map[string]string) {
	host, hasHost := s.Metadata[ingress.MetadataHost]
	sub, hasSub := s.Metadata[ingress.MetadataSubdomain]

	if hasHost && hasSub {
		ve.addf("service %s: cannot set both metadata.%s and metadata.%s", s.Name, ingress.MetadataHost, ingress.MetadataSubdomain)
	}

	switch {
	case hasSub:
		if err := ingress.ValidateSubdomain(sub); err != nil {
			ve.addf("service %s: %v", s.Name, err)
		} else if other, ok := subSeen[sub]; ok {
			ve.addf("services %s and %s: duplicate metadata.subdomain %q", other, s.Name, sub)
		} else {
			subSeen[sub] = s.Name
		}
	case hasHost:
		if h, err := ingress.ValidateHost(host); err != nil {
			ve.addf("service %s: %v", s.Name, err)
		} else if other, ok := hostSeen[h]; ok {
			ve.addf("services %s and %s: duplicate metadata.host %q", other, s.Name, h)
		} else {
			hostSeen[h] = s.Name
		}
	}

	if hasHost || hasSub {
		if !s.Network {
			ve.addf("service %s: public routing requires network: true", s.Name)
		}
		if effectiveBackendPort(s, defs) == 0 {
			ve.addf("service %s: public routing requires a port_forwards entry or a health_check port", s.Name)
		}
	}
}

// effectiveBackendPort mirrors traefik.backendPort: the agent proxies to the
// first port forward's VM port, falling back to the (merged) health-check port.
// Keep these two implementations in agreement so a config that passes enricher
// validation also renders a route at the agent.
func effectiveBackendPort(spec ServiceSpec, defs Defaults) int {
	if len(spec.PortForwards) > 0 {
		return spec.PortForwards[0].VMPort
	}
	if hc := mergeHealthCheck(spec.HealthCheck, defs.HealthCheck); hc != nil && hc.Port > 0 {
		return hc.Port
	}
	return 0
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
		for _, volume := range svc.Volumes {
			if volume.SizeBytes <= 0 {
				ve.addf("service %s volume %s: size_bytes must be positive", svc.Name, volume.Name)
			}
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
				Code:    WarnHealthCheckWithoutNetwork,
				Message: fmt.Sprintf("service %s has health check but network is disabled", svc.Name),
			})
		}

		_, hasHost := svc.Metadata[ingress.MetadataHost]
		_, hasSub := svc.Metadata[ingress.MetadataSubdomain]
		if hasHost || hasSub {
			if len(svc.PortForwards) == 0 || svc.PortForwards[0].HostPort == 0 {
				warns = append(warns, Warn{
					Code:    WarnRemoteRoutingNoHostPort,
					Message: fmt.Sprintf("service %s requests routing but has no first port_forwards host port; it cannot participate in remote multi-node routing", svc.Name),
				})
			}
		}
	}

	return warns
}
