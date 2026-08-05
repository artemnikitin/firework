package reconciler

import "fmt"

// FailureStage identifies the blocking host stage that prevented convergence.
// It is intentionally a small, stable set so agents can publish bounded
// condition and reason values without parsing error strings.
type FailureStage string

const (
	FailureStageNetwork FailureStage = "network"
	FailureStageVM      FailureStage = "vm"
)

// StageError retains the reconciliation failure stage through aggregate
// errors. The underlying error remains available through errors.As/Is.
type StageError struct {
	Stage FailureStage
	Err   error
}

func (e *StageError) Error() string {
	return fmt.Sprintf("%s stage: %v", e.Stage, e.Err)
}

func (e *StageError) Unwrap() error { return e.Err }

func stageError(stage FailureStage, err error) error {
	return &StageError{Stage: stage, Err: err}
}

// HasFailureStage reports whether any member of a wrapped or joined error is
// tagged with stage. This walks every branch explicitly because errors.As
// returns only the first matching value from a joined error, while one
// reconciliation can contain both network and VM failures.
func HasFailureStage(err error, stage FailureStage) bool {
	if err == nil {
		return false
	}
	if typed, ok := err.(*StageError); ok && typed.Stage == stage {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if HasFailureStage(child, stage) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return HasFailureStage(wrapped.Unwrap(), stage)
	}
	return false
}
