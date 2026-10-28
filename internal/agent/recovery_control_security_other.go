//go:build !darwin && !linux

package agent

import (
	"fmt"
	"os"
)

func recoveryControlOwned(os.FileInfo, bool) bool { return false }

func recoveryControlSameOwner(os.FileInfo, os.FileInfo) bool { return false }

func acquireRecoveryControlLock(string) (*os.File, error) {
	return nil, fmt.Errorf("recovery control sockets are unsupported on this platform")
}
