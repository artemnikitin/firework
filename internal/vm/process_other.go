//go:build !linux

package vm

import "fmt"

func (osProcessInspector) Inspect(_ int) (processIdentity, error) {
	return processIdentity{}, errProcessNotFound
}

func (osProcessInspector) FindByArguments(_, _ string) ([]processIdentity, error) {
	return nil, fmt.Errorf("process discovery is unsupported on this platform")
}
