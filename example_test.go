package try_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nodivbyzero/try"
)

// ExampleDo shows the minimal usage: a function that succeeds on the first
// call requires no options at all.
func ExampleDo() {
	ctx := context.Background()

	val, err := try.Do(ctx, func(ctx context.Context) (string, error) {
		return "hello", nil
	})
	fmt.Println(val, err)
	// Output:
	// hello <nil>
}

// ExampleDo_transientFailure demonstrates retrying a flaky operation.
// WithRetryIf restricts retries to known transient errors — a best practice
// for production code so that validation and auth failures fail fast.
func ExampleDo_transientFailure() {
	ctx := context.Background()

	attempt := 0
	val, err := try.Do(ctx, func(ctx context.Context) (int, error) {
		attempt++
		if attempt < 3 {
			return 0, errors.New("connection reset by peer")
		}
		return 42, nil
	},
		try.WithAttempts(5),
		try.WithInitialDelay(time.Millisecond),
		try.WithRetryIf(func(err error) bool {
			return err.Error() == "connection reset by peer"
		}),
	)
	fmt.Println(val, err)
	// Output:
	// 42 <nil>
}

// ExampleDo_permanentError shows how to stop the retry loop immediately for
// errors that will never succeed on retry (auth failures, bad input, etc.).
// Permanent unwraps transparently so errors.Is / errors.As work normally.
func ExampleDo_permanentError() {
	ctx := context.Background()

	calls := 0
	_, err := try.Do(ctx, func(ctx context.Context) (string, error) {
		calls++
		return "", try.Permanent(errors.New("invalid API key"))
	}, try.WithAttempts(5))

	fmt.Println(calls, err)
	// Output:
	// 1 invalid API key
}

// ExampleDo_onRetry demonstrates the OnRetry callback, which fires before
// each wait and receives the attempt number, error, and upcoming delay.
// It is not called on the final attempt since no retry will follow.
func ExampleDo_onRetry() {
	ctx := context.Background()

	attempt := 0
	try.Do(ctx, func(ctx context.Context) (int, error) { //nolint:errcheck
		attempt++
		if attempt < 3 {
			return 0, errors.New("unavailable")
		}
		return 0, nil
	},
		try.WithAttempts(5),
		try.WithInitialDelay(time.Millisecond),
		try.WithOnRetry(func(info try.RetryInfo) {
			slog.Info("retrying", "attempt", info.Attempt, "error", info.Err)
		}),
	)
	fmt.Println("completed after", attempt, "attempts")
	// Output:
	// completed after 3 attempts
}

// rateLimitErr implements RetryAfterer to specify a custom wait duration.
type rateLimitErr struct{ wait time.Duration }

func (e rateLimitErr) Error() string             { return "rate limited" }
func (e rateLimitErr) RetryAfter() time.Duration { return e.wait }

// ExampleDo_retryAfter shows how an error can specify its own wait duration
// via the RetryAfterer interface — useful for honouring HTTP 429 headers.
func ExampleDo_retryAfter() {
	ctx := context.Background()
	attempt := 0

	val, err := try.Do(ctx, func(ctx context.Context) (string, error) {
		attempt++
		if attempt == 1 {
			return "", rateLimitErr{wait: 5 * time.Millisecond}
		}
		return "ok", nil
	}, try.WithAttempts(3))

	fmt.Println(val, err)
	// Output:
	// ok <nil>
}

// ExampleDo_equalJitter shows the EqualJitter strategy, which guarantees at
// least half the computed backoff — useful when a minimum wait time matters.
func ExampleDo_equalJitter() {
	ctx := context.Background()

	attempt := 0
	val, err := try.Do(ctx, func(ctx context.Context) (string, error) {
		attempt++
		if attempt < 3 {
			return "", errors.New("service unavailable")
		}
		return "recovered", nil
	},
		try.WithAttempts(5),
		try.WithInitialDelay(time.Millisecond),
		try.WithMaxDelay(100*time.Millisecond),
		try.WithJitter(try.EqualJitter),
	)
	fmt.Println(val, err)
	// Output:
	// recovered <nil>
}

// ExampleDo_infiniteRetry shows WithInfiniteRetry, which retries until the
// function succeeds, a Permanent error is returned, or the context is cancelled.
// Always pair it with a context deadline.
func ExampleDo_infiniteRetry() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	attempt := 0
	val, err := try.Do(ctx, func(ctx context.Context) (string, error) {
		attempt++
		if attempt < 4 {
			return "", errors.New("not ready")
		}
		return "ready", nil
	},
		try.WithInfiniteRetry(),
		try.WithInitialDelay(time.Millisecond),
	)
	fmt.Println(val, err)
	// Output:
	// ready <nil>
}

// ExampleDo_allErrors shows WithAllErrors, which collects every attempt error
// into *AttemptErrors. errors.Is and errors.As traverse the full history.
func ExampleDo_allErrors() {
	ctx := context.Background()

	sentinel := errors.New("quota exceeded")
	attempt := 0

	_, err := try.Do(ctx, func(ctx context.Context) (int, error) {
		attempt++
		if attempt == 2 {
			return 0, sentinel // one specific error among several
		}
		return 0, errors.New("generic failure")
	},
		try.WithAttempts(3),
		try.WithAllErrors(),
		try.WithInitialDelay(time.Millisecond),
	)

	var ae *try.AttemptErrors
	fmt.Println(errors.As(err, &ae))   // *AttemptErrors accessible
	fmt.Println(errors.Is(err, sentinel)) // sentinel reachable anywhere in history
	// Output:
	// true
	// true
}

// ExampleDo_withTimeout shows WithTimeout, which sets a per-attempt deadline
// independent of the overall context deadline.
func ExampleDo_withTimeout() {
	// Overall budget: 5s. Each attempt gets at most 100ms.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	attempt := 0
	val, err := try.Do(ctx, func(ctx context.Context) (string, error) {
		attempt++
		if attempt < 3 {
			// Simulate a slow response that exceeds the per-attempt timeout.
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(10 * time.Second):
				return "", errors.New("slow")
			}
		}
		return "fast enough", nil
	},
		try.WithAttempts(5),
		try.WithTimeout(10*time.Millisecond),
		try.WithInitialDelay(time.Millisecond),
		try.WithRetryIf(func(err error) bool {
			return errors.Is(err, context.DeadlineExceeded)
		}),
	)
	fmt.Println(val, err)
	// Output:
	// fast enough <nil>
}

// ExampleDo_attemptsForError shows WithAttemptsForError, which caps retries
// for a specific error independently of the global attempt budget.
func ExampleDo_attemptsForError() {
	ctx := context.Background()

	var ErrRateLimit = errors.New("rate limited")
	calls := 0

	_, err := try.Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		return 0, ErrRateLimit
	},
		try.WithAttempts(10),                       // global budget: 10
		try.WithAttemptsForError(2, ErrRateLimit),  // but stop after 2 rate-limit hits
		try.WithInitialDelay(time.Millisecond),
	)

	fmt.Println(calls, errors.Is(err, ErrRateLimit))
	// Output:
	// 2 true
}

// ExampleDo_delayFunc shows WithDelayFunc replacing the built-in backoff
// with a custom linear schedule.
func ExampleDo_delayFunc() {
	ctx := context.Background()
	var observedAttempts []int

	attempt := 0
	try.Do(ctx, func(ctx context.Context) (int, error) { //nolint:errcheck
		attempt++
		if attempt < 3 {
			return 0, errors.New("fail")
		}
		return 0, nil
	},
		try.WithAttempts(5),
		try.WithDelayFunc(func(a int, _ error) time.Duration {
			observedAttempts = append(observedAttempts, a)
			return time.Duration(a) * time.Millisecond // linear: 1ms, 2ms, …
		}),
	)

	fmt.Println(observedAttempts)
	// Output:
	// [1 2]
}

// ExampleIsPermanent shows that IsPermanent reports whether an error was
// wrapped with Permanent, even through additional wrapping layers.
func ExampleIsPermanent() {
	base := errors.New("fatal")
	perm := try.Permanent(base)
	wrapped := fmt.Errorf("outer: %w", perm)

	fmt.Println(try.IsPermanent(perm))
	fmt.Println(try.IsPermanent(wrapped))
	fmt.Println(try.IsPermanent(base))
	// Output:
	// true
	// true
	// false
}

// ExamplePermanent shows that Permanent unwraps cleanly, so the original
// error remains inspectable via errors.Is after the loop exits.
func ExamplePermanent() {
	sentinel := errors.New("fatal")
	wrapped := try.Permanent(sentinel)

	fmt.Println(errors.Is(wrapped, sentinel))
	// Output:
	// true
}
