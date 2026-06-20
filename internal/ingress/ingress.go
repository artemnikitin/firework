// Package ingress holds the shared DNS normalization, validation, and
// hostname-resolution rules used to turn service routing metadata into a
// public Traefik hostname.
//
// It is deliberately dependency-neutral (standard library only) so that
// internal/config, internal/enricher, and internal/traefik can all share one
// implementation rather than duplicate slightly different rules.
//
// Routing model:
//   - metadata["subdomain"]: a single DNS label (e.g. "tenant-1"). The final
//     hostname is "<subdomain>.<ingress_domain>", where ingress_domain is the
//     deployment-owned suffix. Requires a configured ingress domain.
//   - metadata["host"]: an exact DNS hostname, used verbatim. Retained for
//     backward compatibility and custom/internal-host routing.
//
// A service may set at most one of these keys. An absent key means "no route";
// a present-but-empty key is an error.
package ingress

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// MaxLabelLen is the maximum length of a single DNS label (RFC 1035).
	MaxLabelLen = 63
	// MaxNameLen is the maximum length of a full DNS name.
	MaxNameLen = 253
)

// labelRE matches a single lowercase RFC 1123-style DNS label: it must start
// and end with an alphanumeric character and may contain hyphens in between.
var labelRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// MetadataSubdomain is the metadata key for a deployment-portable single-label
// subdomain.
const MetadataSubdomain = "subdomain"

// MetadataHost is the metadata key for an exact, verbatim hostname.
const MetadataHost = "host"

// NormalizeDomain validates and normalizes a deployment ingress domain. It
// trims surrounding whitespace, removes one trailing root dot, lowercases the
// result, and validates it as a DNS name. It rejects schemes, ports, paths,
// query/fragment characters, wildcards, empty labels, over-long labels, and
// invalid hyphen placement. The normalized domain is returned on success.
func NormalizeDomain(raw string) (string, error) {
	d := strings.TrimSpace(raw)
	if d == "" {
		return "", fmt.Errorf("ingress domain is empty")
	}
	d = strings.TrimSuffix(d, ".")
	d = strings.ToLower(d)
	if err := validateDNSName(d); err != nil {
		return "", fmt.Errorf("invalid ingress domain %q: %w", raw, err)
	}
	return d, nil
}

// ValidateSubdomain checks that sub is exactly one lowercase RFC 1123 DNS
// label. Unlike a domain, a subdomain is validated strictly: dots and
// uppercase characters are rejected rather than normalized away.
func ValidateSubdomain(sub string) error {
	if strings.TrimSpace(sub) == "" {
		return fmt.Errorf("subdomain is empty")
	}
	if strings.Contains(sub, ".") {
		return fmt.Errorf("subdomain %q must be exactly one DNS label (no dots)", sub)
	}
	if len(sub) > MaxLabelLen {
		return fmt.Errorf("subdomain %q exceeds %d characters", sub, MaxLabelLen)
	}
	if !labelRE.MatchString(sub) {
		return fmt.Errorf("subdomain %q is not a valid lowercase DNS label", sub)
	}
	return nil
}

// ValidateHost validates an exact hostname and returns its normalized form
// (whitespace trimmed, one trailing root dot removed, lowercased). It rejects
// backticks and other Traefik rule-injection characters before the value can
// be interpolated into a Host(...) rule.
func ValidateHost(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("host is empty")
	}
	h := strings.TrimSpace(raw)
	h = strings.TrimSuffix(h, ".")
	h = strings.ToLower(h)
	// Reject rule-injection and other clearly invalid characters up front so
	// the value can never break out of a Host(`...`) rule.
	if strings.ContainsAny(h, "`$(){}[]<>\"'\\| \t\r\n") {
		return "", fmt.Errorf("host %q contains invalid characters", raw)
	}
	if err := validateDNSName(h); err != nil {
		return "", fmt.Errorf("invalid host %q: %w", raw, err)
	}
	return h, nil
}

// ComposeFQDN joins a validated subdomain and a normalized domain and verifies
// the result fits the DNS name length limit.
func ComposeFQDN(subdomain, domain string) (string, error) {
	fqdn := subdomain + "." + domain
	if len(fqdn) > MaxNameLen {
		return "", fmt.Errorf("composed hostname %q exceeds %d characters", fqdn, MaxNameLen)
	}
	return fqdn, nil
}

// Resolve computes the public hostname for a service from its routing metadata
// and the deployment ingress domain.
//
// It returns ("", nil) only when the service requests no route (neither
// "subdomain" nor "host" is present). Any present-but-invalid configuration —
// both keys set, an empty value, an invalid subdomain/host, or a subdomain
// without an ingress domain — returns an error naming the service.
func Resolve(serviceName string, metadata map[string]string, ingressDomain string) (string, error) {
	host, hasHost := metadata[MetadataHost]
	sub, hasSub := metadata[MetadataSubdomain]

	switch {
	case !hasHost && !hasSub:
		return "", nil
	case hasHost && hasSub:
		return "", fmt.Errorf("service %s: cannot set both metadata.%s and metadata.%s", serviceName, MetadataHost, MetadataSubdomain)
	case hasHost:
		h, err := ValidateHost(host)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", serviceName, err)
		}
		return h, nil
	default: // hasSub
		if err := ValidateSubdomain(sub); err != nil {
			return "", fmt.Errorf("service %s: %w", serviceName, err)
		}
		if strings.TrimSpace(ingressDomain) == "" {
			return "", fmt.Errorf("service %s: metadata.subdomain %q requires the agent ingress_domain to be set", serviceName, sub)
		}
		dom, err := NormalizeDomain(ingressDomain)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", serviceName, err)
		}
		fqdn, err := ComposeFQDN(sub, dom)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", serviceName, err)
		}
		return fqdn, nil
	}
}

// validateDNSName validates an already-lowercased, trailing-dot-stripped DNS
// name: at least one label, each a valid RFC 1123 label, total length within
// the DNS limit, and no scheme/port/path/wildcard characters.
func validateDNSName(d string) error {
	if d == "" {
		return fmt.Errorf("name is empty")
	}
	if len(d) > MaxNameLen {
		return fmt.Errorf("name exceeds %d characters", MaxNameLen)
	}
	for _, bad := range []string{"://", "/", ":", "*", "?", "#", "@", "%"} {
		if strings.Contains(d, bad) {
			return fmt.Errorf("name contains invalid sequence %q", bad)
		}
	}
	labels := strings.Split(d, ".")
	for _, l := range labels {
		switch {
		case l == "":
			return fmt.Errorf("name has an empty label")
		case len(l) > MaxLabelLen:
			return fmt.Errorf("name has a label longer than %d characters", MaxLabelLen)
		case !labelRE.MatchString(l):
			return fmt.Errorf("name has invalid label %q", l)
		}
	}
	return nil
}
