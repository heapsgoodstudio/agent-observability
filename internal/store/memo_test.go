package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoSameKeyWithinTTLRunsOnce(t *testing.T) {
	m := New(50 * time.Millisecond)
	var calls int32
	run := func() (any, error) {
		atomic.AddInt32(&calls, 1)
		return "v", nil
	}
	v1, err1 := m.Do("k", run)
	v2, err2 := m.Do("k", run)
	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v, %v", err1, err2)
	}
	if v1 != "v" || v2 != "v" {
		t.Fatalf("values: %v, %v, want v, v", v1, v2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestMemoErrorNotCached(t *testing.T) {
	m := New(time.Minute)
	calls := 0
	run := func() (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		return "v", nil
	}
	if _, err := m.Do("k", run); err == nil {
		t.Fatal("want error on first call")
	}
	v, err := m.Do("k", run)
	if err != nil {
		t.Fatalf("second call error = %v, want nil", err)
	}
	if v != "v" {
		t.Fatalf("v = %v, want v", v)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (a cached error would leave this at 1)", calls)
	}
}

func TestMemoDifferentKeysRunSeparately(t *testing.T) {
	m := New(time.Minute)
	calls := 0
	run := func() (any, error) {
		calls++
		return calls, nil
	}
	v1, _ := m.Do("a", run)
	v2, _ := m.Do("b", run)
	if v1 == v2 {
		t.Fatalf("expected different keys to run separately, both returned %v", v1)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestMemoConcurrentSameKeySharesInFlightResult(t *testing.T) {
	m := New(time.Minute)
	var calls int32
	release := make(chan struct{})
	run := func() (any, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return "v", nil
	}

	const n = 5
	var wg sync.WaitGroup
	results := make([]any, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := m.Do("k", run)
			results[i] = v
			errs[i] = err
		}(i)
	}
	time.Sleep(20 * time.Millisecond) // let every goroutine reach Do before unblocking run
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (concurrent same-key calls should share one in-flight run)", got)
	}
	for i, v := range results {
		if errs[i] != nil {
			t.Errorf("results[%d] error = %v", i, errs[i])
		}
		if v != "v" {
			t.Errorf("results[%d] = %v, want v", i, v)
		}
	}
}
