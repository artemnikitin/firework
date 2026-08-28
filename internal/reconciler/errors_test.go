package reconciler

import (
	"errors"
	"fmt"
	"testing"

	"github.com/artemnikitin/firework/internal/vm"
)

func TestHasFailureStageFindsWrappedAndJoinedStages(t *testing.T) {
	err := fmt.Errorf("apply: %w", errors.Join(
		stageError(FailureStageNetwork, errors.New("tap failed")),
		stageError(FailureStageVM, errors.New("launch failed")),
	))
	if !HasFailureStage(err, FailureStageNetwork) {
		t.Fatal("network stage was lost through aggregate error")
	}
	if !HasFailureStage(err, FailureStageVM) {
		t.Fatal("VM stage was lost through aggregate error")
	}
}

// The abort must survive the exact wrapping the apply path performs. A
// hand-built error proves nothing: the defect this guards against is a link in
// that chain flattening the error with %v.
func TestIsIncompleteSeesThroughTheProductionErrorShape(t *testing.T) {
	abort := stageError(FailureStageVM,
		fmt.Errorf("starting VM: %w", fmt.Errorf("service app: %w", vm.ErrStartAborted)))
	inProgress := stageError(FailureStageVM,
		fmt.Errorf("starting VM: %w", fmt.Errorf("service api is in state starting: %w", vm.ErrStartInProgress)))
	genuine := stageError(FailureStageNetwork, errors.New("tap creation failed"))

	wrapApply := func(errs ...error) error {
		return combineErrors([]error{fmt.Errorf("reconciliation had %d error(s): %w", len(errs), errors.Join(errs...))})
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not incomplete", err: nil, want: false},
		{name: "single abort", err: wrapApply(abort), want: true},
		{name: "abort plus concurrent start", err: wrapApply(abort, inProgress), want: true},
		{name: "abort mixed with a genuine failure", err: wrapApply(abort, genuine), want: false},
		{name: "genuine failure alone", err: wrapApply(genuine), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsIncomplete(test.err); got != test.want {
				t.Fatalf("IsIncomplete = %v, want %v (err: %v)", got, test.want, test.err)
			}
		})
	}
}
