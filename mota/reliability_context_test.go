package mota

import (
	"context"
	"testing"
	"time"
)

func TestStateFinalizationContextDetachesCancellationAndAddsDeadline(t *testing.T) {
	t.Parallel()
	type contextKey struct{}
	requestCtx, cancelRequest := context.WithCancel(
		context.WithValue(context.Background(), contextKey{}, "kept"))
	cancelRequest()

	ctx, cancel := stateFinalizationContext(requestCtx)
	defer cancel()
	if ctx.Err() != nil {
		t.Fatalf("finalization context inherited cancellation: %v", ctx.Err())
	}
	if ctx.Value(contextKey{}) != "kept" {
		t.Fatalf("request value was lost: %v", ctx.Value(contextKey{}))
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("finalization context must have a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > stateFinalizationTimeout {
		t.Fatalf("deadline remaining = %v, want (0, %v]", remaining, stateFinalizationTimeout)
	}
}
