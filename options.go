package try

import "time"

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
