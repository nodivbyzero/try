# try

A small, generic Go library for retrying fallible operations with exponential backoff and pluggable jitter strategies.

## Features

- **Generic** — works with any return type via `Do[T]`
- **Exponential backoff** with pluggable jitter — Full Jitter (default) or Equal Jitter
- **`Permanent` errors** — stop retrying immediately for non-recoverable failures
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
| `WithRetryIf(fn func(error) bool)` | `Predicate` | retry all | Return `false` to stop retrying for a given error |
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

## Comparison

| Capability | `avast/retry-go` | `nodivbyzero/try` |
|---|---|---|
| Exponential backoff | Yes (`BackOffDelay`) | Yes |
| Jitter | Yes (`RandomDelay`, combinable) | Yes (`FullJitter`, `EqualJitter`) |
| Context cancellation | Yes | Yes (wraps last error) |
| Retry predicates | Yes (`RetryIf`, `AttemptsForError`) | Yes (`WithRetryIf`) |
| Error aggregation | Yes (all errors wrapped) | No (last error only) |
| Generic return values | Yes (`DoWithData[T]`) | Yes (`Do[T]`) — unified API |
| Hooks / callbacks | `OnRetry(n uint, err error)` | `OnRetry(RetryInfo)` with delay |
| Infinite retry | Yes (`Attempts(0)`) | Yes (`WithInfiniteRetry()`) |
| Custom delay function | Yes (`DelayTypeFunc`) | No |
| `RetryAfterer` interface | No | Yes |
| Testable clock | No | Yes (`WithClock`) |
| Zero dependencies | Yes | Yes |
| API style | `retry.New(opts...).Do(fn)` | `try.Do(ctx, fn, opts...)` |

**When to use `avast/retry-go`:** you need error aggregation across all attempts,
custom delay functions, or per-error attempt budgets (`AttemptsForError`).

**When to use `nodivbyzero/try`:** you want a context-first generic API, need to honour
`Retry-After` headers from error values, or want a testable clock for unit tests without real sleeps.

## License

MIT
