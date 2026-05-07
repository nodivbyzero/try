// Package try provides a generic, context-aware retry loop with exponential
// backoff and pluggable jitter strategies.
//
// # Basic usage
//
//	val, err := try.Do(ctx, func(ctx context.Context) (string, error) {
//	    return callExternalAPI(ctx)
//	})
//
// Do retries up to 5 times by default, waiting between attempts using
// exponential backoff capped at 30 seconds.
//
// # Options
//
// Behaviour is configured through functional options:
//
//	try.Do(ctx, fn,
//	    try.WithAttempts(10),
//	    try.WithInitialDelay(500*time.Millisecond),
//	    try.WithMaxDelay(2*time.Minute),
//	    try.WithJitter(try.EqualJitter),
//	    try.WithRetryIf(isTransient),
//	    try.WithOnRetry(func(info try.RetryInfo) {
//	        slog.Warn("retrying",
//	            "attempt", info.Attempt,
//	            "delay",   info.Delay,
//	            "error",   info.Err,
//	        )
//	    }),
//	)
//
// [WithMaxDelay] overrides the default 30s cap on any single wait, which is
// useful when integrating with slow services or enforcing strict SLAs.
//
// [WithOnRetry] registers a callback invoked before each wait, receiving a
// [RetryInfo] with the 1-based attempt number, the error, and the delay about
// to be taken. The callback is not called on the final attempt since no retry
// will follow. Use it for structured logging, metrics, or tracing.
//
// # Default retry behaviour
//
// By default Do retries on every error except [context.Canceled],
// [context.DeadlineExceeded], and errors wrapped with [Permanent]. This means
// validation errors, auth failures, and malformed-payload errors are retried
// unless explicitly excluded. For production use, always supply a [WithRetryIf]
// predicate to restrict retries to genuinely transient failures:
//
//	try.WithRetryIf(func(err error) bool {
//	    var httpErr *HTTPError
//	    if errors.As(err, &httpErr) {
//	        return httpErr.StatusCode >= 500 // never retry 4xx
//	    }
//	    return true
//	})
//
// # Infinite retry
//
// Use [WithInfiniteRetry] to retry until the function succeeds, a [Permanent]
// error is returned, or the context is cancelled. Always pair this with a
// context deadline to prevent runaway retries:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
//	defer cancel()
//
//	try.Do(ctx, fn, try.WithInfiniteRetry())
//
// # Custom delay function
//
// [WithDelayFunc] replaces the built-in exponential backoff entirely with a
// caller-supplied function. It receives the 1-based attempt number that just
// failed and the error it returned:
//
//	// Fixed 500ms delay:
//	try.WithDelayFunc(func(attempt int, err error) time.Duration {
//	    return 500 * time.Millisecond
//	})
//
//	// Linear backoff: 1s, 2s, 3s, …
//	try.WithDelayFunc(func(attempt int, err error) time.Duration {
//	    return time.Duration(attempt) * time.Second
//	})
//
//	// Error-dependent delay:
//	try.WithDelayFunc(func(attempt int, err error) time.Duration {
//	    if errors.Is(err, ErrThrottled) {
//	        return 10 * time.Second
//	    }
//	    return time.Duration(attempt) * 200 * time.Millisecond
//	})
//
// [RetryAfterer] on the error still takes precedence over [WithDelayFunc].
//
// # Error aggregation
//
// By default Do returns only the last attempt's error. Use [WithAllErrors] to
// collect every attempt error into an [AttemptErrors] value. Because
// AttemptErrors implements Unwrap() []error, the full history is searchable
// with errors.Is and errors.As:
//
//	_, err := try.Do(ctx, fn, try.WithAttempts(3), try.WithAllErrors())
//
//	var ae *try.AttemptErrors
//	if errors.As(err, &ae) {
//	    for i, e := range ae.Unwrap() {
//	        log.Printf("attempt %d: %v", i+1, e)
//	    }
//	}
//
//	// errors.Is still works across the full history:
//	if errors.Is(err, ErrRateLimit) { ... }
//
// # Per-attempt timeout
//
// [WithTimeout] sets a deadline on each individual call to fn, independent of
// the parent context deadline which governs the entire retry operation. If fn
// exceeds the timeout its context is cancelled and the attempt is retried:
//
//	try.Do(ctx, fn,
//	    try.WithAttempts(5),
//	    try.WithTimeout(500*time.Millisecond), // each attempt gets 500ms
//	)
//
// The parent context deadline still governs the overall operation. A slow fn
// that hits the per-attempt timeout receives context.DeadlineExceeded on its
// child context; the parent context remains live and the retry loop continues.
//
// # Per-error attempt budgets
//
// [WithAttemptsForError] sets an independent retry cap for a specific error.
// When that error is returned and its budget is exhausted, the loop stops
// immediately — even if the global [WithAttempts] budget has remaining
// attempts. Multiple calls accumulate independent budgets:
//
//	try.Do(ctx, fn,
//	    try.WithAttempts(10),
//	    try.WithAttemptsForError(2, ErrRateLimit),   // stop after 2 rate-limit hits
//	    try.WithAttemptsForError(3, ErrUnavailable), // stop after 3 unavailable hits
//	)
//
// Matching uses [errors.Is], so sentinel errors and wrapped errors both work.
//
// # Stopping immediately
//
// Wrap an error with [Permanent] to stop the loop without exhausting all
// attempts. The underlying error is unwrapped, so [errors.Is] and [errors.As]
// work normally on the value returned by Do:
//
//	return 0, try.Permanent(err)
//
// Use [IsPermanent] to inspect whether an error came from a permanent failure
// without unwrapping it manually:
//
//	if try.IsPermanent(err) {
//	    // do not retry at a higher level
//	}
//
// # Retry-After support
//
// If an error implements [RetryAfterer], its RetryAfter duration is used
// instead of the computed backoff, making it straightforward to honour
// HTTP 429 Retry-After headers:
//
//	func (e *RateLimitError) RetryAfter() time.Duration { return e.RetryIn }
//
// # Jitter strategies
//
// Two strategies are available via [WithJitter]:
//
//   - [FullJitter] (default): delay is drawn uniformly from [1ms, cap).
//     Maximally spreads concurrent retriers; recommended for most cases.
//
//   - [EqualJitter]: delay is cap/2 + rand[0, cap/2).
//     Guarantees at least half the exponential backoff; useful when a
//     minimum wait time is required.
//
// # Testing
//
// Inject a custom [Clock] via [WithClock] to control time in unit tests
// without real sleeps.
package try
