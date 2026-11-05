package touchid

import (
	"context"
	"errors"
	"testing"
)

// The sheet itself needs someone at the laptop; an ended ctx must not show it.
func TestAskWithAnEndedContextShowsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Ask(ctx, "never shown"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
