//go:build !linux

package vm

import "fmt"

// processInspectionSupported reports whether the host exposes the process
// identity Firework needs to prove VM ownership. Without it a launch cannot be
// verified, so this platform is development-only.
const processInspectionSupported = false

func (osProcessInspector) Inspect(_ int) (processIdentity, error) {
	return processIdentity{}, errProcessNotFound
}

func (osProcessInspector) FindByArguments(_, _ string) ([]processIdentity, error) {
	return nil, fmt.Errorf("process discovery is unsupported on this platform")
}
