package try

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// testClock allows us to control time in tests
type testClock struct {
	afterChan chan time.Time
}

func (t *testClock) After(d time.Duration) <-chan time.Time {
	return t.afterChan
}

func (t *testClock) Now() time.Time {
	return time.Now()
}

func TestDo_Success(t *testing.T) {
	ctx := context.Background()
	callCount := 0

	val, err := Do(ctx, func(ctx context.Context) (string, error) {
		callCount++
		if callCount < 3 {
			return "", errors.New("temporary error")
		}
		return "success", nil
	}, WithAttempts(5), WithInitialDelay(1*time.Millisecond))

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != "success" {
		t.Errorf("expected success, got %s", val)
	}
	if callCount != 3 {
		t.Errorf("expected 3 calls, got %d", callCount)
	}
}

func TestDo_PermanentError(t *testing.T) {
	ctx := context.Background()
	callCount := 0

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		callCount++
		return 0, Permanent(errors.New("fatal"))
	}, WithAttempts(5))

	if callCount != 1 {
		t.Errorf("expected 1 call for permanent error, got %d", callCount)
	}
	if err.Error() != "fatal" {
		t.Errorf("expected 'fatal' error, got %v", err)
	}
}

func TestDo_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clk := &testClock{afterChan: make(chan time.Time)}

	// Cancel context immediately
	cancel()

	_, err := Do(ctx, func(ctx context.Context) (string, error) {
		return "", errors.New("fail")
	}, WithClock(clk))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context canceled error, got %v", err)
	}
}

type retryAfterError struct{ d time.Duration }

func (e retryAfterError) Error() string             { return "retry after" }
func (e retryAfterError) RetryAfter() time.Duration { return e.d }

func TestDo_RetryAfter(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time)}

	errTriggered := make(chan bool)

	go func() {
		_, _ = Do(ctx, func(ctx context.Context) (int, error) {
			errTriggered <- true
			return 0, retryAfterError{d: 10 * time.Hour}
		}, WithClock(clk), WithAttempts(2))
	}()

	<-errTriggered
	// At this point, the library is sitting in the 'select' block 
	// waiting for the 10 hour duration.
	
	select {
	case clk.afterChan <- time.Now():
		// Success: the library accepted our manual tick
	case <-time.After(1 * time.Second):
		t.Fatal("library did not respect retry-after or clock")
	}
}

func TestDo_Generics(t *testing.T) {
	ctx := context.Background()
	
	// Test with a struct
	type User struct{ ID int }
	val, _ := Do(ctx, func(ctx context.Context) (User, error) {
		return User{ID: 42}, nil
	})

	if val.ID != 42 {
		t.Errorf("generic type User not preserved, got %v", val)
	}
}
func TestDo_OnRetry(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}

	// Immediately unblock every clock wait.
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	retryErr := errors.New("transient")
	var infos []RetryInfo

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		return 0, retryErr
	},
		WithAttempts(4),
		WithClock(clk),
		WithOnRetry(func(info RetryInfo) {
			infos = append(infos, info)
		}),
	)

	// Callback should fire once per retried attempt (not on the final failure).
	if len(infos) != 3 {
		t.Fatalf("expected 3 OnRetry calls, got %d", len(infos))
	}

	for i, info := range infos {
		// Attempt numbers should be 1-based and sequential.
		if info.Attempt != i+1 {
			t.Errorf("call %d: expected Attempt=%d, got %d", i, i+1, info.Attempt)
		}
		// Error passed to callback must be the original error.
		if !errors.Is(info.Err, retryErr) {
			t.Errorf("call %d: expected err=%v, got %v", i, retryErr, info.Err)
		}
		// Delay must be positive (1ms floor enforced by calculateNextDelay).
		if info.Delay <= 0 {
			t.Errorf("call %d: expected positive delay, got %v", i, info.Delay)
		}
	}

	// The final error should still be the original transient error.
	if !errors.Is(err, retryErr) {
		t.Errorf("expected final err=%v, got %v", retryErr, err)
	}
}

func TestDo_TinyInitialDelay_NoPanic(t *testing.T) {
	// Regression test: Int64N panics if n <= 0. With a 1ns InitialDelay the
	// computed cap can underflow to zero before jitter is applied. Verify both
	// strategies handle sub-millisecond delays without panicking.
	for _, strategy := range []JitterStrategy{FullJitter, EqualJitter} {
		ctx := context.Background()
		clk := &testClock{afterChan: make(chan time.Time, 10)}
		for i := 0; i < 10; i++ {
			clk.afterChan <- time.Now()
		}

		// Should not panic regardless of strategy.
		_, err := Do(ctx, func(ctx context.Context) (int, error) {
			return 0, errors.New("fail")
		},
			WithAttempts(3),
			WithInitialDelay(1*time.Nanosecond),
			WithJitter(strategy),
			WithClock(clk),
		)
		if err == nil {
			t.Errorf("strategy %v: expected error, got nil", strategy)
		}
	}
}

func TestDo_WithMaxDelay(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	maxDelay := 5 * time.Millisecond
	var infos []RetryInfo

	_, _ = Do(ctx, func(ctx context.Context) (int, error) {
		return 0, errors.New("fail")
	},
		WithAttempts(5),
		WithInitialDelay(1*time.Millisecond),
		WithMaxDelay(maxDelay),
		WithJitter(EqualJitter), // EqualJitter floor is cap/2, easier to assert upper bound
		WithClock(clk),
		WithOnRetry(func(info RetryInfo) {
			infos = append(infos, info)
		}),
	)

	for _, info := range infos {
		if info.Delay > maxDelay {
			t.Errorf("attempt %d: delay %v exceeded MaxDelay %v", info.Attempt, info.Delay, maxDelay)
		}
	}
}

func TestDo_ContextCancellation_WrapsLastErr(t *testing.T) {
	// When the context is cancelled during a wait, the returned error must
	// wrap both ctx.Err() and the last operation error so callers can inspect
	// either via errors.Is / errors.As.
	ctx, cancel := context.WithCancel(context.Background())
	clk := &testClock{afterChan: make(chan time.Time)} // never ticks

	opErr := errors.New("operation failed")

	done := make(chan error, 1)
	go func() {
		_, err := Do(ctx, func(ctx context.Context) (int, error) {
			return 0, opErr
		}, WithClock(clk), WithAttempts(5))
		done <- err
	}()

	// Let one attempt run, then cancel while Do is waiting on the clock.
	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-done

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("expected errors.Is(err, opErr), got %v", err)
	}
}

func TestDo_LargeInitialDelay_NoPanic(t *testing.T) {
	// Regression test: InitialDelay near math.MaxInt64/2 would overflow int64
	// in a single bit-shift multiply, producing a negative cap and bypassing
	// MaxDelay. Iterative doubling must clamp to MaxDelay before overflow.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	maxDelay := 10 * time.Second
	var infos []RetryInfo

	_, _ = Do(ctx, func(ctx context.Context) (int, error) {
		return 0, errors.New("fail")
	},
		WithAttempts(5),
		WithInitialDelay(1<<62*time.Nanosecond), // enormous InitialDelay
		WithMaxDelay(maxDelay),
		WithClock(clk),
		WithOnRetry(func(info RetryInfo) {
			infos = append(infos, info)
		}),
	)

	for _, info := range infos {
		if info.Delay > maxDelay {
			t.Errorf("attempt %d: delay %v exceeded MaxDelay %v", info.Attempt, info.Delay, maxDelay)
		}
	}
}

func TestDo_InfiniteRetry_SucceedsEventually(t *testing.T) {
	// WithInfiniteRetry should keep retrying until the function succeeds.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 20)}
	for i := 0; i < 20; i++ {
		clk.afterChan <- time.Now()
	}

	attempt := 0
	val, err := Do(ctx, func(ctx context.Context) (int, error) {
		attempt++
		if attempt < 10 {
			return 0, errors.New("not yet")
		}
		return attempt, nil
	},
		WithInfiniteRetry(),
		WithClock(clk),
	)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != 10 {
		t.Errorf("expected val=10, got %d", val)
	}
}

func TestDo_InfiniteRetry_StopsOnContextCancel(t *testing.T) {
	// WithInfiniteRetry must stop when the context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	clk := &testClock{afterChan: make(chan time.Time)}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		return 0, errors.New("always fails")
	},
		WithInfiniteRetry(),
		WithClock(clk),
	)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestDo_InfiniteRetry_StopsOnPermanent(t *testing.T) {
	// WithInfiniteRetry must still honour Permanent errors.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 5)}

	calls := 0
	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		return 0, Permanent(errors.New("fatal"))
	},
		WithInfiniteRetry(),
		WithClock(clk),
	)

	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
	if err == nil || err.Error() != "fatal" {
		t.Errorf("expected 'fatal' error, got %v", err)
	}
}

func TestIsPermanent(t *testing.T) {
	base := errors.New("fatal")

	if IsPermanent(nil) {
		t.Error("IsPermanent(nil) should be false")
	}
	if IsPermanent(base) {
		t.Error("IsPermanent(unwrapped err) should be false")
	}
	if !IsPermanent(Permanent(base)) {
		t.Error("IsPermanent(Permanent(err)) should be true")
	}
	// Must work through an additional wrapping layer.
	wrapped := fmt.Errorf("outer: %w", Permanent(base))
	if !IsPermanent(wrapped) {
		t.Error("IsPermanent should work through fmt.Errorf wrapping")
	}
}

func TestDo_AttemptsForError_ExhaustsBeforeGlobal(t *testing.T) {
	// The per-error budget (2) is smaller than the global budget (10).
	// The loop should stop after 2 attempts for the target error.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	targetErr := errors.New("rate limited")
	calls := 0

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		return 0, targetErr
	},
		WithAttempts(10),
		WithAttemptsForError(2, targetErr),
		WithClock(clk),
	)

	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
	if !errors.Is(err, targetErr) {
		t.Errorf("expected targetErr, got %v", err)
	}
}

func TestDo_AttemptsForError_OtherErrorsUnaffected(t *testing.T) {
	// A per-error budget for errA should not affect retries for errB.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	errA := errors.New("error A")
	errB := errors.New("error B")
	calls := 0

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		if calls < 4 {
			return 0, errB // not the budgeted error — retries freely
		}
		return 0, errA // budgeted to 2 attempts; first hit exhausts it
	},
		WithAttempts(10),
		WithAttemptsForError(1, errA),
		WithClock(clk),
	)

	if calls != 4 {
		t.Errorf("expected 4 calls, got %d", calls)
	}
	if !errors.Is(err, errA) {
		t.Errorf("expected errA, got %v", err)
	}
}

func TestDo_AttemptsForError_MultipleTargets(t *testing.T) {
	// Independent budgets for two different errors.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 20)}
	for i := 0; i < 20; i++ {
		clk.afterChan <- time.Now()
	}

	errA := errors.New("error A")
	errB := errors.New("error B")
	calls := 0

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		if calls%2 == 0 {
			return 0, errB
		}
		return 0, errA
	},
		WithAttempts(20),
		WithAttemptsForError(3, errA), // stops after 3 errA hits
		WithAttemptsForError(3, errB),
		WithClock(clk),
	)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDo_AttemptsForError_WorksWithWrappedErrors(t *testing.T) {
	// errors.Is matching should work through fmt.Errorf wrapping.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	sentinel := errors.New("sentinel")
	calls := 0

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		return 0, fmt.Errorf("wrapped: %w", sentinel)
	},
		WithAttempts(10),
		WithAttemptsForError(2, sentinel),
		WithClock(clk),
	)

	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel in err chain, got %v", err)
	}
}

func TestDo_WithTimeout_SlowFnIsRetried(t *testing.T) {
	// A fn that blocks longer than AttemptTimeout should have its context
	// cancelled and the attempt counted as a failure to be retried.
	ctx := context.Background()
	attempts := 0

	val, err := Do(ctx, func(ctx context.Context) (int, error) {
		attempts++
		select {
		case <-ctx.Done():
			// Attempt timed out — return the context error so it gets retried.
			return 0, ctx.Err()
		case <-time.After(10 * time.Second):
			return 42, nil
		}
	},
		WithAttempts(3),
		WithTimeout(10*time.Millisecond),
		WithInitialDelay(time.Millisecond),
		WithRetryIf(func(err error) bool {
			// Retry on per-attempt timeout but not on parent cancellation.
			return errors.Is(err, context.DeadlineExceeded)
		}),
	)

	// All 3 attempts should have timed out individually.
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
	_ = val
	_ = err
}

func TestDo_WithTimeout_FastFnSucceeds(t *testing.T) {
	// A fn that completes within the timeout should succeed normally.
	ctx := context.Background()

	val, err := Do(ctx, func(ctx context.Context) (string, error) {
		return "ok", nil
	},
		WithTimeout(100*time.Millisecond),
	)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != "ok" {
		t.Errorf("expected ok, got %s", val)
	}
}

func TestDo_WithTimeout_ParentCancelStopsLoop(t *testing.T) {
	// Cancelling the parent context must stop the loop even with a per-attempt
	// timeout set — the parent deadline governs the whole operation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		return 0, errors.New("fail")
	},
		WithAttempts(5),
		WithTimeout(1*time.Second),
		WithInitialDelay(time.Millisecond),
	)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestDo_WithAllErrors_CollectsAllAttempts(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	err1 := errors.New("attempt 1 failed")
	err2 := errors.New("attempt 2 failed")
	err3 := errors.New("attempt 3 failed")
	errs := []error{err1, err2, err3}
	call := 0

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		e := errs[call]
		call++
		return 0, e
	},
		WithAttempts(3),
		WithAllErrors(),
		WithClock(clk),
	)

	var ae *AttemptErrors
	if !errors.As(err, &ae) {
		t.Fatalf("expected *AttemptErrors, got %T: %v", err, err)
	}
	if len(ae.Unwrap()) != 3 {
		t.Errorf("expected 3 errors, got %d", len(ae.Unwrap()))
	}
	// All individual errors accessible via errors.Is.
	for _, target := range errs {
		if !errors.Is(err, target) {
			t.Errorf("expected errors.Is(err, %v) to be true", target)
		}
	}
}

func TestDo_WithAllErrors_ErrorMessage(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 5)}
	for i := 0; i < 5; i++ {
		clk.afterChan <- time.Now()
	}

	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		return 0, errors.New("boom")
	},
		WithAttempts(2),
		WithAllErrors(),
		WithClock(clk),
	)

	msg := err.Error()
	if !strings.Contains(msg, "attempt 1:") || !strings.Contains(msg, "attempt 2:") {
		t.Errorf("expected message to contain attempt labels, got: %s", msg)
	}
}

func TestDo_WithAllErrors_StopsOnPermanent(t *testing.T) {
	// Permanent errors should still short-circuit; only the errors up to that
	// point are aggregated.
	ctx := context.Background()

	call := 0
	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		call++
		if call == 2 {
			return 0, Permanent(errors.New("fatal"))
		}
		return 0, errors.New("transient")
	},
		WithAttempts(5),
		WithAllErrors(),
		WithInitialDelay(time.Millisecond),
	)

	var ae *AttemptErrors
	if !errors.As(err, &ae) {
		t.Fatalf("expected *AttemptErrors, got %T", err)
	}
	if len(ae.Unwrap()) != 2 {
		t.Errorf("expected 2 errors (transient + fatal), got %d", len(ae.Unwrap()))
	}
	if call != 2 {
		t.Errorf("expected 2 calls, got %d", call)
	}
}

func TestDo_WithoutAllErrors_ReturnsLastOnly(t *testing.T) {
	// Default behaviour: only last error returned, no AttemptErrors wrapper.
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 5)}
	for i := 0; i < 5; i++ {
		clk.afterChan <- time.Now()
	}

	lastErr := errors.New("last")
	call := 0
	_, err := Do(ctx, func(ctx context.Context) (int, error) {
		call++
		if call < 3 {
			return 0, errors.New("earlier")
		}
		return 0, lastErr
	},
		WithAttempts(3),
		WithClock(clk),
	)

	if !errors.Is(err, lastErr) {
		t.Errorf("expected lastErr, got %v", err)
	}
	var ae *AttemptErrors
	if errors.As(err, &ae) {
		t.Error("expected no *AttemptErrors without WithAllErrors")
	}
}
