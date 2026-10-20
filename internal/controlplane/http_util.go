package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Request body limits. Each endpoint is capped before its first read or
// decode, so an unauthenticated caller cannot make a server buffer more than
// that endpoint can legitimately need.
const (
	// Enrollment is pre-authentication: a node ID, a bootstrap token, and a
	// single CSR.
	maxEnrollRequestBytes = 64 << 10
	// Register, renew, and state carry a CSR or small identity/capacity
	// fields; heartbeat alone uses maxRegistryRequestBytes.
	maxRegistryControlRequestBytes = 256 << 10
	// GitHub does not deliver webhook payloads larger than 25 MB, so this
	// admits every payload GitHub can send while bounding what the
	// pre-signature read buffers.
	maxGitHubWebhookBytes = 25 << 20
	maxLoginFormBytes     = 4 << 10
)

func limitBody(w http.ResponseWriter, r *http.Request, limit int64) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
}

func isRequestTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

func readJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func headerValue(headers http.Header, key string) string {
	for k, values := range headers {
		if strings.EqualFold(k, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
