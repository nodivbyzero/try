# try

A small, generic Go library for retrying fallible operations with exponential backoff and pluggable jitter strategies.

## Features

- **Generic** — works with any return type via `Do[T]`
- **Exponential backoff** with pluggable jitter — Full Jitter (default) or Equal Jitter
- **`Permanent` errors** — stop retrying immediately for non-recoverable failures
- **`IsPermanent(err)`** — inspect whether an error originated from a permanent failure
- **Per-error budgets** — `WithAttemptsForError(n, err)` caps retries for a specific error independently of the global limit
- **Per-attempt timeout** — `WithTimeout(d)` cancels a single slow attempt without affecting the overall retry budget
- **Error aggregation** — `WithAllErrors()` collects every attempt error; inspect the full history via `errors.Is` / `errors.As`
- **Bounded error history** — `WithMaxErrorHistory(n)` caps the retained errors via a ring buffer; essential when combining `WithInfiniteRetry` and `WithAllErrors`
- **Custom delay function** — `WithDelayFunc` replaces the built-in backoff with any schedule: fixed, linear, or error-dependent
- **Jitter window cap** — `WithMaxJitter(d)` constrains spread independently of the backoff cap
- **`RetryAfterer` interface** — errors can specify their own wait duration (e.g. HTTP 429)
- **Custom predicates** — decide per-error whether to retry
- **Testable** — injectable `Clock` interface for time-travel in unit tests
- **Infinite retry** — `WithInfiniteRetry()` retries until success or context cancellation
- **Context-aware** — honours cancellation and deadline at every wait point

## Installation

```bash
go get github.com/nodivbyzero/try
```

## Quick Start

```go
val, err := try.Do(ctx, func(ctx context.Context) (string, error) {
    return callExternalAPI(ctx)
})
```

`Do` retries up to 5 times by default, with exponential backoff capped at 30 seconds.

> **Default retry behaviour:** `Do` retries on *every* error except `context.Canceled`,
> `context.DeadlineExceeded`, and errors wrapped with [`Permanent`](#stopping-immediately-permanent).
> This means validation errors, auth failures, and malformed-payload errors will be retried
> unless you opt out. For production use, always supply a [`WithRetryIf`](#best-practice-filtering-retryable-errors)
> predicate to avoid wasting attempts on non-transient failures.


## Infinite Retry

Use `WithInfiniteRetry` to retry until the function succeeds, a `Permanent` error
is returned, or the context is cancelled. Always pair it with a context deadline:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()

val, err := try.Do(ctx, fetchUser,
    try.WithInfiniteRetry(),
    try.WithOnRetry(func(info try.RetryInfo) {
        slog.Warn("retrying", "attempt", info.Attempt, "error", info.Err)
    }),
)
// err will be context.DeadlineExceeded (wrapping the last op error) if
// the function never succeeds within the timeout.
```

## Options

Each option is a functional setter for a field on `Config`:

| Option | `Config` Field | Default | Description |
|---|---|---|---|
| `WithAttempts(n int)` | `MaxAttempts` | `5` | Total attempts including the first call |
| `WithInfiniteRetry()` | `MaxAttempts` | — | Retry until success, `Permanent` error, or context cancellation |
| `WithInitialDelay(d time.Duration)` | `InitialDelay` | `200ms` | Starting backoff; doubles each attempt up to `MaxDelay` |
| `WithMaxDelay(d time.Duration)` | `MaxDelay` | `30s` | Upper bound on any single wait regardless of backoff growth |
| `WithJitter(s JitterStrategy)` | `Jitter` | `FullJitter` | Jitter strategy: `FullJitter` or `EqualJitter` |
| `WithDelayFunc(fn func(int, error) time.Duration)` | `DelayFunc` | nil | Replace built-in backoff entirely; `RetryAfterer` still takes precedence |
| `WithMaxJitter(d time.Duration)` | `MaxJitter` | disabled | Cap jitter window independently of backoff cap |
| `WithRetryIf(fn func(error) bool)` | `Predicate` | retry all | Return `false` to stop retrying for a given error |
| `WithAttemptsForError(n int, err error)` | `ErrorBudgets` | — | Cap retries for a specific error; multiple calls accumulate |
| `WithTimeout(d time.Duration)` | `AttemptTimeout` | disabled | Per-attempt deadline; cancelled attempts are retried |
| `WithAllErrors()` | `AllErrors` | false | Aggregate all attempt errors into `*AttemptErrors` |
| `WithMaxErrorHistory(n int)` | `MaxErrorHistory` | unlimited | Cap retained errors to last `n`; ring-buffer evicts oldest first |
| `WithOnRetry(fn func(RetryInfo))` | `OnRetry` | nil | Callback fired before each wait — use for logging or metrics |
| `WithClock(clk Clock)` | `Clock` | `time.After` | Injectable clock for time-travel in tests |

```go
val, err := try.Do(ctx, fetchUser,
    try.WithAttempts(10),                  // Config.MaxAttempts  = 10
    try.WithInitialDelay(500*time.Millisecond),   // Config.InitialDelay = 500ms
    try.WithMaxDelay(2*time.Minute),       // Config.MaxDelay     = 2m
    try.WithJitter(try.EqualJitter),       // Config.Jitter       = EqualJitter
    try.WithRetryIf(func(err error) bool {
        return isTransient(err)            // Config.Predicate
    }),
    try.WithOnRetry(func(info try.RetryInfo) {
        slog.Warn("retrying",
            "attempt", info.Attempt,
            "delay",   info.Delay,
            "error",   info.Err,
        )
    }),
)
```

## Stopping Immediately: `Permanent`

Wrap an error with `try.Permanent` to stop the retry loop without waiting for remaining attempts:

```go
val, err := try.Do(ctx, func(ctx context.Context) (*User, error) {
    u, err := db.Find(ctx, id)
    if errors.Is(err, sql.ErrNoRows) {
        return nil, try.Permanent(err) // no point retrying
    }
    return u, err
})
```

The underlying error is unwrapped, so `errors.Is` / `errors.As` work normally on the returned error.

Use `IsPermanent` to check whether an error came from a permanent failure at any call site — without unwrapping manually:

```go
val, err := try.Do(ctx, fn)
if try.IsPermanent(err) {
    // non-recoverable — do not retry at a higher level
    return err
}
```

`IsPermanent` works through additional wrapping layers, so `fmt.Errorf("%w", permanentErr)` is correctly detected.

## Respecting `Retry-After`: `RetryAfterer`

If your error type knows how long the caller should wait (e.g. a rate-limit response), implement the `RetryAfterer` interface and `try` will use that duration instead of the computed backoff:

```go
type RateLimitError struct {
    RetryIn time.Duration
}

func (e RateLimitError) Error() string             { return "rate limited" }
func (e RateLimitError) RetryAfter() time.Duration { return e.RetryIn }
```

The duration is still capped at `MaxDelay`.

## Backoff Algorithm

The exponential cap for attempt `n` is `min(MaxDelay, InitialDelay × 2^(n−1))`. The jitter strategy then derives the actual wait from that cap:

| Strategy | Formula | Behaviour |
|---|---|---|
| `FullJitter` (default) | `rand[0, cap)` | Maximally spreads retriers; may produce very short waits |
| `EqualJitter` | `cap/2 + rand[0, cap/2)` | Guarantees at least half the backoff; softer lower bound |

Both strategies enforce a 1ms minimum floor. The Full Jitter approach is recommended by [AWS](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) for avoiding thundering herd; Equal Jitter is preferable when a minimum wait time matters.

## Jitter Window Cap

By default the jitter window equals the full backoff cap, so `FullJitter` with
a 30s cap draws delays from the entire `[0, 30s)` range. Use `WithMaxJitter`
to constrain the spread independently — useful for services where you want long
base delays but minimal pile-up variance:

```go
// Base delay grows to 30s, but jitter is capped at 500ms.
// Delays will be in [0, 500ms) regardless of how large the backoff has grown.
try.Do(ctx, fn,
    try.WithInitialDelay(1*time.Second),
    try.WithMaxDelay(30*time.Second),
    try.WithMaxJitter(500*time.Millisecond),
)
```

When `MaxJitter` is larger than the current backoff cap it has no effect —
the backoff cap is always the effective ceiling.

## Custom Delay Function

`WithDelayFunc` replaces the built-in exponential backoff with any schedule
you need. The function receives the 1-based attempt number and the error that
caused the failure:

```go
// Fixed delay — no backoff at all.
try.WithDelayFunc(func(attempt int, err error) time.Duration {
    return 500 * time.Millisecond
})

// Linear backoff: 1s, 2s, 3s, …
try.WithDelayFunc(func(attempt int, err error) time.Duration {
    return time.Duration(attempt) * time.Second
})

// Error-dependent: long wait for throttle errors, short for others.
try.WithDelayFunc(func(attempt int, err error) time.Duration {
    if errors.Is(err, ErrThrottled) {
        return 10 * time.Second
    }
    return time.Duration(attempt) * 200 * time.Millisecond
})
```

`WithDelayFunc` takes precedence over `WithInitialDelay`, `WithMaxDelay`, and
`WithJitter`. [`RetryAfterer`](#respecting-retry-after-retryafterer) on the
error still takes precedence over `WithDelayFunc`.

## Error Aggregation

By default `Do` returns only the last attempt's error. Use `WithAllErrors` to
collect every attempt error into `*AttemptErrors`, which implements
`Unwrap() []error` for Go 1.20+ multi-error unwrapping. This lets you inspect
the full failure history with `errors.Is` and `errors.As`:

```go
_, err := try.Do(ctx, fn,
    try.WithAttempts(3),
    try.WithAllErrors(),
)

var ae *try.AttemptErrors
if errors.As(err, &ae) {
    for i, e := range ae.Unwrap() {
        slog.Warn("attempt failed", "attempt", i+1, "error", e)
    }
}

// errors.Is traverses all attempt errors, not just the last one.
if errors.Is(err, ErrRateLimit) {
    // at least one attempt was rate-limited
}
```

`WithAllErrors` is opt-in — the default behaviour (last error only) is
unchanged and has no allocation overhead.

### Bounding history with `WithMaxErrorHistory`

When combining `WithAllErrors` with `WithInfiniteRetry`, `allErrs` grows
indefinitely until context cancellation. Use `WithMaxErrorHistory(n)` to cap
it to the last `n` errors via a ring buffer — oldest errors are evicted first:

```go
try.Do(ctx, fn,
    try.WithInfiniteRetry(),
    try.WithAllErrors(),
    try.WithMaxErrorHistory(50), // retain only the last 50 errors
)
```

For bounded `WithAttempts` runs the history is pre-allocated to the exact
attempt count, so no reallocation occurs even without a cap.

## Per-Attempt Timeout

`WithTimeout` sets a deadline on each individual call to `fn`, distinct from the
parent context deadline which governs the entire retry operation. If `fn` blocks
longer than the timeout its context is cancelled and the attempt is retried:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // overall budget
defer cancel()

val, err := try.Do(ctx, callSlowService,
    try.WithAttempts(5),
    try.WithTimeout(2*time.Second), // each attempt gets 2s
    try.WithRetryIf(func(err error) bool {
        // Retry per-attempt timeouts; stop on other errors.
        return errors.Is(err, context.DeadlineExceeded)
    }),
)
```

The parent context deadline still governs the overall operation — if the parent
is cancelled mid-retry the loop stops immediately.

## Per-Error Attempt Budgets

`WithAttemptsForError` sets an independent retry cap for a specific error value.
When that error is returned and its budget is exhausted, the loop stops immediately
— even if the global `WithAttempts` budget has remaining attempts.

```go
var ErrRateLimit = errors.New("rate limited")
var ErrUnavailable = errors.New("service unavailable")

val, err := try.Do(ctx, fn,
    try.WithAttempts(10),
    try.WithAttemptsForError(2, ErrRateLimit),    // stop after 2 rate-limit hits
    try.WithAttemptsForError(3, ErrUnavailable),  // stop after 3 unavailable hits
)
```

Multiple `WithAttemptsForError` calls accumulate independent budgets. Matching
uses `errors.Is`, so wrapped errors are detected correctly:

```go
// This will match ErrRateLimit even through fmt.Errorf wrapping.
return 0, fmt.Errorf("upstream: %w", ErrRateLimit)
```

## Best Practice: Filtering Retryable Errors

Because `Do` retries all errors by default, use `WithRetryIf` to restrict retries
to genuinely transient failures in production code:

```go
// HTTP example: only retry on 5xx or network errors, never on 4xx.
val, err := try.Do(ctx, fetchUser,
    try.WithRetryIf(func(err error) bool {
        var httpErr *HTTPError
        if errors.As(err, &httpErr) {
            return httpErr.StatusCode >= 500
        }
        return true // retry network/timeout errors
    }),
)
```

```go
// gRPC example: retry on Unavailable and DeadlineExceeded, not on
// InvalidArgument, NotFound, PermissionDenied, etc.
try.WithRetryIf(func(err error) bool {
    switch status.Code(err) {
    case codes.Unavailable, codes.ResourceExhausted:
        return true
    default:
        return false
    }
})
```

Errors that should **never** be retried: validation failures, authentication errors,
not-found responses, and any error that will produce the same result on every attempt.
Wrap these with [`Permanent`](#stopping-immediately-permanent) or filter them out via
`WithRetryIf` to fail fast and avoid unnecessary load on downstream services.

## Examples

Runnable examples for all major features are in [`example_test.go`](./example_test.go)
and render on [pkg.go.dev](https://pkg.go.dev/github.com/nodivbyzero/try). They cover:

- `ExampleDo` — minimal zero-config usage
- `ExampleDo_transientFailure` — flaky call with `WithRetryIf` predicate
- `ExampleDo_permanentError` — early exit with `Permanent`
- `ExampleDo_onRetry` — structured logging via `WithOnRetry`
- `ExampleDo_retryAfter` — honouring `RetryAfterer` on rate-limit errors
- `ExampleDo_equalJitter` — `EqualJitter` with `WithMaxDelay`
- `ExamplePermanent` — `errors.Is` through the `Permanent` wrapper

## Testing

Pass a `testClock` via `WithClock` to control time without real sleeps:

```go
type testClock struct {
    ch chan time.Time
}
func (c *testClock) After(d time.Duration) <-chan time.Time { return c.ch }
func (c *testClock) Now() time.Time                         { return time.Now() }

clk := &testClock{ch: make(chan time.Time)}
go func() {
    _, _ = try.Do(ctx, alwaysFails, try.WithClock(clk), try.WithAttempts(3))
}()
clk.ch <- time.Now() // advance past first wait instantly
```


## License

MIT
