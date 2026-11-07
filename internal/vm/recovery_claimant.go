package vm

import (
	"errors"
	"fmt"
)

// recoveryClaimantError distinguishes observed process or launch ownership
// from a failure to finish inspecting that ownership.
type recoveryClaimantError struct{ message string }

func (e *recoveryClaimantError) Error() string { return e.message }

func recoveryClaimant(format string, args ...any) error {
	return &recoveryClaimantError{message: fmt.Sprintf(format, args...)}
}

// IsRecoveryClaimant reports whether an absence check observed a process or
// systemd launch claiming the VM. An unavailable inspection is not evidence
// of a claimant and must not cancel an already accepted forget retry.
func IsRecoveryClaimant(err error) bool {
	var claimant *recoveryClaimantError
	return errors.As(err, &claimant)
}
