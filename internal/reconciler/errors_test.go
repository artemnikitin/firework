package reconciler

import (
	"errors"
	"fmt"
	"testing"
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
