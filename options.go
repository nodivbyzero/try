package try

import "time"

// WithAttempts sets the maximum number of attempts, including the first call.
// See also WithInfiniteRetry.
func WithAttempts(n int) Option {
	return func(c *Config) { c.MaxAttempts = n }
}

func WithInitialDelay(d time.Duration) Option {
	return func(c *Config) { c.InitialDelay = d }
}

func WithRetryIf(p func(error) bool) Option {
	return func(c *Config) { c.Predicate = p }
}

func WithClock(clk Clock) Option {
	return func(c *Config) { c.Clock = clk }
}

func WithJitter(s JitterStrategy) Option {
	return func(c *Config) { c.Jitter = s }
}

// WithOnRetry registers fn to be called before each wait.
// It is not called on the final attempt since no retry will occur.
func WithOnRetry(fn func(RetryInfo)) Option {
	return func(c *Config) { c.OnRetry = fn }
}

func WithMaxDelay(d time.Duration) Option {
	return func(c *Config) { c.MaxDelay = d }
}

// WithInfiniteRetry removes the attempt cap entirely. Do will retry until the
// function succeeds, a Permanent error is returned, or the context is cancelled.
// Always pair this with a context deadline to prevent runaway retries.
func WithInfiniteRetry() Option {
	return func(c *Config) { c.MaxAttempts = 0 }
}

// WithAttemptsForError sets a maximum retry count for a specific error value.
// When err is returned and its per-error budget is exhausted, the loop stops
// immediately — even if the global MaxAttempts budget has remaining attempts.
// Multiple calls accumulate independent budgets for different error values.
// Matching uses errors.Is, so sentinel errors and wrapped errors both work.
func WithAttemptsForError(n int, target error) Option {
	return func(c *Config) {
		c.ErrorBudgets = append(c.ErrorBudgets, errorBudget{
			target:    target,
			remaining: n - 1, // n attempts means n-1 retries after the first call
		})
	}
}

// WithTimeout sets a deadline on each individual call to fn, distinct from
// the parent context deadline which governs the entire retry operation.
// If fn exceeds the timeout, its context is cancelled and the attempt is
// retried (subject to the usual retry rules). Zero disables per-attempt
// timeouts, which is the default.
func WithTimeout(d time.Duration) Option {
	return func(c *Config) { c.AttemptTimeout = d }
}

// WithAllErrors enables error aggregation. When set, Do collects every
// attempt error and returns them as *AttemptErrors, which implements
// Unwrap() []error so that errors.Is and errors.As traverse the full
// history. Without this option only the last error is returned.
func WithAllErrors() Option {
	return func(c *Config) { c.AllErrors = true }
}
