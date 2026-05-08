package try

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// RetryAfterer allows errors to specify a custom wait duration.
type RetryAfterer interface {
	RetryAfter() time.Duration
}

// Clock interface allows for "time-travel" in unit tests.
type Clock interface {
	After(d time.Duration) <-chan time.Time
	Now() time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) Now() time.Time                         { return time.Now() }

// JitterStrategy controls how randomness is applied to the backoff delay.
type JitterStrategy int

const (
	// FullJitter draws the delay uniformly from [1ms, cap), minimising
	// correlated retries at the cost of potentially very short waits.
	// This is the default and is recommended for most use cases.
	FullJitter JitterStrategy = iota

	// EqualJitter uses cap/2 + rand[0, cap/2), guaranteeing at least half
	// the exponential backoff while still spreading load across retriers.
	EqualJitter
)

// RetryInfo carries context about a failed attempt, passed to the OnRetry callback.
type RetryInfo struct {
	Attempt   int           // 1-based attempt number that just failed
	Err       error         // error returned by the attempt
	Delay     time.Duration // how long Do will wait before the next attempt
}

// Config holds the internal state for the retry operation.
type Config struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Clock        Clock
	Predicate    func(error) bool
	Jitter       JitterStrategy
	// OnRetry is called before each wait. It is not called on the final
	// attempt since no retry will occur.
	OnRetry func(RetryInfo)
	// ErrorBudgets holds per-error attempt limits set by WithAttemptsForError.
	// Each entry is checked independently; the first exhausted budget stops retries
	// for that error, and counts against the global MaxAttempts budget too.
	ErrorBudgets []errorBudget
	// AttemptTimeout limits how long a single call to fn may run.
	// Zero means no per-attempt timeout (default). Distinct from the parent
	// context deadline, which governs the entire retry operation.
	AttemptTimeout time.Duration
	// AllErrors enables error aggregation. When true, Do collects every
	// attempt error and returns them joined via errors.Join so that
	// errors.Is / errors.As can inspect the full history.
	AllErrors bool
	// DelayFunc overrides the built-in backoff algorithm entirely.
	// When set, it is called instead of calculateNextDelay. RetryAfterer
	// on the error is still respected before DelayFunc is consulted.
	DelayFunc func(attempt int, err error) time.Duration
	// MaxJitter caps the jitter window independently of the backoff cap.
	// Zero means no independent jitter cap — the full backoff cap is used
	// as the jitter window (default behaviour).
	MaxJitter time.Duration
}

// AttemptErrors is the joined error type returned when WithAllErrors is set.
// It implements Unwrap() []error for Go 1.20+ multi-error unwrapping, so
// errors.Is and errors.As traverse every attempt's error.
type AttemptErrors struct {
	errs []error
}

func (e *AttemptErrors) Error() string {
	msgs := make([]string, len(e.errs))
	for i, err := range e.errs {
		msgs[i] = fmt.Sprintf("attempt %d: %v", i+1, err)
	}
	return strings.Join(msgs, "; ")
}

// Unwrap returns all attempt errors for errors.Is / errors.As traversal.
func (e *AttemptErrors) Unwrap() []error { return e.errs }

// Option defines functional configuration for the retry.
type Option func(*Config)

// defaultConfig returns sensible defaults. MaxAttempts of 5 means the
// function is called at most 5 times. Set MaxAttempts to 0 for infinite retry.
func defaultConfig() *Config {
	return &Config{
		MaxAttempts: 5,
		InitialDelay: 200 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Clock:       realClock{},
	}
}

// Do is the generic entry point for retrying a function.
func Do[T any](ctx context.Context, fn func(ctx context.Context) (T, error), opts ...Option) (T, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	var zero T
	var lastErr error
	var allErrs []error // populated when cfg.AllErrors is set
	// infinite is true when MaxAttempts == 0: retry until success or context
	// cancellation. Otherwise the loop runs for exactly MaxAttempts iterations.
	infinite := cfg.MaxAttempts == 0
	for attempt := 1; infinite || attempt <= cfg.MaxAttempts; attempt++ {
		// Bail immediately if the parent context is already done before we even
		// call fn. This handles the case where ctx was cancelled before the first
		// attempt, or was cancelled during the previous attempt's execution.
		if ctx.Err() != nil {
			ctxErr := fmt.Errorf("%w: last error: %w", ctx.Err(), lastErr)
			if cfg.AllErrors && len(allErrs) > 0 {
				allErrs = append(allErrs, ctxErr)
				return zero, &AttemptErrors{errs: allErrs}
			}
			return zero, ctxErr
		}

		// Wrap the parent context with a per-attempt deadline if configured.
		// The timeout context is always cancelled after fn returns to free
		// resources, regardless of success or failure.
		attemptCtx := ctx
		var cancelAttempt context.CancelFunc
		if cfg.AttemptTimeout > 0 {
			attemptCtx, cancelAttempt = context.WithTimeout(ctx, cfg.AttemptTimeout)
		}

		val, err := fn(attemptCtx)

		if cancelAttempt != nil {
			cancelAttempt()
		}

		if err == nil {
			return val, nil
		}

		lastErr = err
		if cfg.AllErrors {
			allErrs = append(allErrs, err)
		}

		if !shouldRetry(ctx, cfg, err) {
			if cfg.AllErrors {
				return zero, &AttemptErrors{errs: allErrs}
			}
			return zero, err
		}

		// Skip the delay on the final attempt of a bounded run — there is
		// nothing to wait for. Infinite loops always sleep between attempts.
		isFinalAttempt := !infinite && attempt == cfg.MaxAttempts
		if isFinalAttempt {
			break
		}

		// Calculate backoff
		delay := calculateNextDelay(cfg, attempt, err)

		// Fire the OnRetry callback so callers can log or record metrics.
		// Not called on the final attempt of a bounded run since no retry follows.
		if cfg.OnRetry != nil {
			cfg.OnRetry(RetryInfo{Attempt: attempt, Err: err, Delay: delay})
		}

		// Wait for either the delay or context cancellation.
		// Wrap ctx.Err() with the last operation error so callers can inspect
		// both: what the context did (Canceled/DeadlineExceeded) and what the
		// function last returned. errors.Is/As on the wrapped error still
		// surfaces ctx.Err() correctly via Unwrap.
		select {
		case <-ctx.Done():
			return zero, fmt.Errorf("%w: last error: %w", ctx.Err(), lastErr)
		case <-cfg.Clock.After(delay):
			continue
		}
	}

	if cfg.AllErrors && len(allErrs) > 0 {
		return zero, &AttemptErrors{errs: allErrs}
	}
	return zero, lastErr
}

// Permanent wraps an error to signal the loop should stop immediately.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// IsPermanent reports whether err was wrapped with Permanent.
// Useful for inspecting errors after Do returns, without unwrapping manually.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// errorBudget pairs a target error with its remaining retry allowance.
type errorBudget struct {
	target    error
	remaining int
}

// matchBudget returns a pointer to the first budget whose target matches err
// via errors.Is, or nil if no budget applies.
func matchBudget(budgets []errorBudget, err error) *errorBudget {
	for i := range budgets {
		if errors.Is(err, budgets[i].target) {
			return &budgets[i]
		}
	}
	return nil
}

func shouldRetry(ctx context.Context, cfg *Config, err error) bool {
	// Stop if the parent context is done — check ctx.Err() rather than the
	// error value so that per-attempt timeouts (a child context) are not
	// mistaken for parent cancellation. A DeadlineExceeded from a child
	// context should be retried; one from the parent should not.
	if ctx.Err() != nil {
		return false
	}
	var p *permanentError
	if errors.As(err, &p) {
		return false
	}
	if b := matchBudget(cfg.ErrorBudgets, err); b != nil {
		if b.remaining <= 0 {
			return false
		}
		b.remaining--
	}
	if cfg.Predicate != nil {
		return cfg.Predicate(err)
	}
	return true
}

func calculateNextDelay(cfg *Config, attempt int, err error) time.Duration {
	// 1. Check for Retry-After override — takes precedence over everything.
	if ra, ok := err.(RetryAfterer); ok {
		d := ra.RetryAfter()
		if d > cfg.MaxDelay {
			return cfg.MaxDelay
		}
		return d
	}

	// 2. Delegate to the custom delay function if one is configured.
	// The caller is responsible for capping and jitter within their function.
	if cfg.DelayFunc != nil {
		d := cfg.DelayFunc(attempt, err)
		if d < 0 {
			d = 0
		}
		return d
	}

	// 3. Compute exponential cap: min(MaxDelay, InitialDelay * 2^(attempt-1))
	//
	// Use iterative doubling instead of a bit-shift multiply so that overflow
	// is caught explicitly at each step. A single multiply like
	// InitialDelay * (1 << shift) can silently wrap to a negative value when
	// InitialDelay is large, and relying on cap <= 0 to detect that is subtle.
	cap := cfg.InitialDelay
	for i := 1; i < attempt; i++ {
		cap *= 2
		if cap >= cfg.MaxDelay || cap < 0 { // cap < 0 means int64 overflowed
			cap = cfg.MaxDelay
			break
		}
	}
	if cap > cfg.MaxDelay {
		cap = cfg.MaxDelay
	}

	// 4. Apply MaxJitter cap: if set, the jitter window is the lesser of the
	// computed exponential cap and MaxJitter. This allows long deterministic
	// base delays with a small spread (e.g. 30s base ± 500ms jitter).
	if cfg.MaxJitter > 0 && cfg.MaxJitter < cap {
		cap = cfg.MaxJitter
	}

	// 5. Enforce the 1ms floor on the cap *before* passing it to Int64N.
	// rand.Int64N(n) panics if n <= 0, which can happen when InitialDelay is
	// very small (e.g. 1ns) and the computed cap rounds down to zero or below
	// the minimum meaningful range. Clamping here is safe: if cap < 1ms the
	// result would have been floored to 1ms anyway.
	if cap < time.Millisecond {
		cap = time.Millisecond
	}

	// 6. Apply jitter strategy.
	var d time.Duration
	switch cfg.Jitter {
	case EqualJitter:
		// Equal Jitter: cap/2 + rand[0, cap/2)
		// Guarantees at least half the exponential backoff, reducing the chance
		// of very short delays while still spreading retriers over time.
		// After the floor above, cap >= 1ms so half >= 500µs > 0 — no panic risk.
		half := cap / 2
		d = half + time.Duration(rand.Int64N(int64(half)))
	default: // FullJitter
		// Full Jitter: rand[0, cap)
		// Minimises correlated retries at the cost of potentially short waits.
		// cap >= 1ms after the floor above — no panic risk.
		d = time.Duration(rand.Int64N(int64(cap)))
	}

	return d
}
