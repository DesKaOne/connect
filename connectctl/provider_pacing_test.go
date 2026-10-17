package main

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

func TestStartupPacerGivesConcurrentCallersDistinctSlots(t *testing.T) {
	const count = 24
	const interval = 5 * time.Second
	start := time.Date(2026, 10, 17, 23, 0, 0, 0, time.UTC)
	p := newStartupPacer(interval)
	reservations := make([]time.Time, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			reservations[index] = p.reserve(start)
		}(i)
	}
	wg.Wait()
	sort.Slice(reservations, func(i, j int) bool { return reservations[i].Before(reservations[j]) })
	for i, actual := range reservations {
		expected := start.Add(time.Duration(i) * interval)
		if !actual.Equal(expected) {
			t.Fatalf("slot %d at %s; expected %s", i, actual, expected)
		}
	}
}

func TestStartupPacerDisabled(t *testing.T) {
	p := newStartupPacer(0)
	now := time.Date(2026, 10, 17, 23, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if at := p.reserve(now); !at.Equal(now) {
			t.Fatalf("disabled pacer delayed setup: %s", at.Sub(now))
		}
	}
}

func TestStartupPacerWaitHonorsCanceledContext(t *testing.T) {
	p := newStartupPacer(5 * time.Minute)
	p.reserve(time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := time.Now()
	if err := p.wait(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(before) > time.Second {
		t.Fatal("canceling startup wait took too long")
	}
}

func TestProviderStartupIntervalPrecedenceAndBounds(t *testing.T) {
	t.Setenv("STARTUP_INTERVAL", "7")
	parsed, err := docopt.ParseArgs(connectCtlUsage(), []string{
		"provide", "--startup_interval=5", "--max_connection=10", "--concurrency=2",
	}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	interval, err := providerStartupInterval(parsed)
	if err != nil || interval != 5*time.Second {
		t.Fatalf("flag must win over environment: interval=%s err=%v", interval, err)
	}

	envOpts, err := docopt.ParseArgs(connectCtlUsage(), []string{"provide"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	interval, err = providerStartupInterval(envOpts)
	if err != nil || interval != 7*time.Second {
		t.Fatalf("env setting not respected: interval=%s err=%v", interval, err)
	}
	t.Setenv("STARTUP_INTERVAL", "0")
	interval, err = providerStartupInterval(envOpts)
	if err != nil || interval != 0 {
		t.Fatalf("0 must disable pacing: interval=%s err=%v", interval, err)
	}
	for _, bad := range []string{"-1", "601", "1.5", "invalid", ""} {
		if _, err := parseStartupIntervalSeconds(bad); err == nil {
			t.Errorf("accepted invalid STARTUP_INTERVAL %q", bad)
		}
	}
}

func TestProviderRetryBackoffIsStaggered(t *testing.T) {
	if got := providerRetryDelay(0); got != 30*time.Second {
		t.Fatalf("first retry delay %s", got)
	}
	if got := providerRetryDelay(9); got != 39*time.Second {
		t.Fatalf("ninth retry delay %s", got)
	}
	if got := providerRetryDelay(10); got != 30*time.Second {
		t.Fatalf("cycle retry delay %s", got)
	}
}
