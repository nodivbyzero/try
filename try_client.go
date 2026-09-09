package try

import "context"

// Try is a reusable client that holds a set of default [Option] values,
// applied to every call made through [Try.Do]. It exists for callers who
// want consistent retry behaviour across many call sites — for example a
// service struct that configures its retry policy once at construction and
// then calls Do per method — without repeating the same options everywhere.
//
// The zero value is not useful; construct a *Try with [New].
//
// A *Try only holds configuration (a slice of Option) and Do never mutates
// t.defaults, so a single *Try is safe for concurrent use by multiple
// goroutines.
type Try struct {
	defaults []Option
}

// New returns a *Try configured with the given default options. The
// defaults are applied to every call made through the returned [Try.Do],
// before any options supplied at the call site.
func New(opts ...Option) *Try {
	// Copy defensively so later mutation of the caller's slice (if any)
	// can't retroactively change this Try's behaviour.
	defaults := make([]Option, len(opts))
	copy(defaults, opts)
	return &Try{defaults: defaults}
}

// Do calls the package-level [Do], merging t's defaults with the per-call
// opts. Options are applied in order — defaults first, then opts — so where
// a per-call option configures the same [Config] field as one of t's
// defaults, the per-call value wins (last-applied-wins). This is the
// "defaults with overrides" behaviour callers generally expect.
//
// Do declares its own type parameter T, independent of Try's (non-generic)
// receiver. This requires Go 1.27, which added support for generic methods
// on concrete types; see the go.mod minimum version.
func (t *Try) Do[T any](ctx context.Context, fn func(context.Context) (T, error), opts ...Option) (T, error) {
	merged := make([]Option, 0, len(t.defaults)+len(opts))
	merged = append(merged, t.defaults...)
	merged = append(merged, opts...)
	return Do(ctx, fn, merged...)
}
