package reconciler

import (
	"fmt"

	"github.com/artemnikitin/firework/internal/vm"
)

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

// IsIncomplete reports whether a reconciliation error consists *only* of benign
// start-barrier races — a start that was aborted by a concurrent stop or
// remove, or one that collided with another start still preparing volumes.
//
// The distinction matters because of what an ordinary nil return would do. The
// agent advances lastRevision at the end of a successful tick, and the next
// tick then takes the unchanged-revision shortcut and never re-plans, leaving
// an aborted service down until the revision itself changes. So an aborted
// start must neither succeed nor be reported as a hard failure: it is
// incomplete, and the caller retries on the next tick without claiming the
// revision or raising a reconcile_failed condition.
//
// A batch that mixes an abort with a genuine failure is a failure. Both
// classifications leave the revision unchanged; the difference is what the node
// reports.
func IsIncomplete(err error) bool {
	leaves := reconcileLeaves(err, nil)
	if len(leaves) == 0 {
		return false
	}
	for _, leaf := range leaves {
		if !vm.IsStartRace(leaf) {
			return false
		}
	}
	return true
}

// reconcileLeaves flattens an aggregate error into the individual errors it was
// built from. Joined branches are walked; a plain wrapped chain is followed to
// its innermost error, which is where a sentinel lives.
func reconcileLeaves(err error, out []error) []error {
	for err != nil {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				out = reconcileLeaves(child, out)
			}
			return out
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok || wrapped.Unwrap() == nil {
			return append(out, err)
		}
		err = wrapped.Unwrap()
	}
	return out
}
