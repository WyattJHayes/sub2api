package repository

import (
	"errors"
	"testing"
)

func TestCreateRunRollsBackWhenBindingIsIncomplete(t *testing.T) {
	err := requireCompletePairBindings(2, 1)
	if !errors.Is(err, ErrIncompleteExperimentBinding) {
		t.Fatalf("requireCompletePairBindings() error = %v, want ErrIncompleteExperimentBinding", err)
	}
}
