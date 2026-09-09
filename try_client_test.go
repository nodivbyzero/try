package try

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTry_Do_DefaultsApplied(t *testing.T) {
	// With no per-call opts, the defaults given to New must govern behaviour.
	ctx := context.Background()
	client := New(WithAttempts(3), WithInitialDelay(time.Millisecond))

	calls := 0
	_, err := client.Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		return 0, errors.New("always fails")
	})

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (from default WithAttempts(3)), got %d", calls)
	}
}

func TestTry_Do_PerCallOverridesDefaults(t *testing.T) {
	// A per-call option targeting the same Config field as a default must
	// win (last-applied-wins), since opts are appended after defaults.
	ctx := context.Background()
	client := New(WithAttempts(2), WithInitialDelay(time.Millisecond))

	calls := 0
	_, err := client.Do(ctx, func(ctx context.Context) (int, error) {
		calls++
		return 0, errors.New("always fails")
	}, WithAttempts(5))

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if calls != 5 {
		t.Errorf("expected 5 calls (per-call WithAttempts(5) overriding default of 2), got %d", calls)
	}
}

func TestTry_Do_UnrelatedDefaultsSurviveOverride(t *testing.T) {
	// Overriding one field via per-call opts must not disturb unrelated
	// defaults (e.g. overriding MaxAttempts shouldn't reset InitialDelay).
	ctx := context.Background()
	clk := &testClock{afterChan: make(chan time.Time, 10)}
	for i := 0; i < 10; i++ {
		clk.afterChan <- time.Now()
	}

	var delays []time.Duration
	client := New(
		WithInitialDelay(50*time.Millisecond),
		WithMaxDelay(50*time.Millisecond),
		WithClock(clk),
		WithOnRetry(func(info RetryInfo) {
			delays = append(delays, info.Delay)
		}),
	)

	_, _ = client.Do(ctx, func(ctx context.Context) (int, error) {
		return 0, errors.New("fail")
	}, WithAttempts(3)) // only override the attempt count

	if len(delays) == 0 {
		t.Fatal("expected at least one recorded delay")
	}
	for _, d := range delays {
		if d > 50*time.Millisecond {
			t.Errorf("delay %v exceeded the default MaxDelay of 50ms; default was lost", d)
		}
	}
}

func TestTry_Do_Success(t *testing.T) {
	ctx := context.Background()
	client := New(WithAttempts(3), WithInitialDelay(time.Millisecond))

	val, err := client.Do(ctx, func(ctx context.Context) (string, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != "ok" {
		t.Errorf("expected 'ok', got %q", val)
	}
}

func TestTry_Do_Generics(t *testing.T) {
	// Do's type parameter must be inferred independently per call site,
	// even though it's declared on the (non-generic) *Try receiver's method.
	ctx := context.Background()
	client := New()

	type User struct{ ID int }
	u, err := client.Do(ctx, func(ctx context.Context) (User, error) {
		return User{ID: 7}, nil
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if u.ID != 7 {
		t.Errorf("expected User{ID: 7}, got %+v", u)
	}

	n, err := client.Do(ctx, func(ctx context.Context) (int, error) {
		return 99, nil
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if n != 99 {
		t.Errorf("expected 99, got %d", n)
	}
}

func TestTry_Do_ConcurrentUse(t *testing.T) {
	// A single *Try must be safe to share across goroutines: Do must never
	// mutate t.defaults, only ever read it into a fresh merged slice.
	ctx := context.Background()
	client := New(WithAttempts(2), WithInitialDelay(time.Millisecond))

	const goroutines = 50
	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			calls := 0
			_, err := client.Do(ctx, func(ctx context.Context) (int, error) {
				calls++
				if calls < 2 {
					return 0, errors.New("transient")
				}
				return i, nil
			}, WithInitialDelay(time.Millisecond)) // per-call opt on a shared client
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: expected success, got %v", i, err)
		}
	}
}
