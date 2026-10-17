package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docopt/docopt-go"
)

// startupPacer limits the rate of *new provider session initialization*.
// CONCURRENCY limits simultaneous operations; the pacer ensures those
// operations cannot all begin at once. One shared pacer is used for initial
// starts and explicit session retries.
type startupPacer struct {
	mu sync.Mutex
	interval time.Duration
	next time.Time
}

func newStartupPacer(interval time.Duration) *startupPacer {
	return &startupPacer{interval: interval}
}

// reserve assigns a distinct start time even when concurrent callers arrive
// simultaneously. Keeping this pure of sleeping makes spacing easy to test.
func (p *startupPacer) reserve(now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.interval <= 0 {
		return now
	}
	at := now
	if p.next.After(at) {
		at = p.next
	}
	p.next = at.Add(p.interval)
	return at
}

func (p *startupPacer) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	startAt := p.reserve(time.Now())
	delay := time.Until(startAt)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// startup_interval is expressed as whole seconds to match --stats_interval.
// 0 disables pacing; defaults to 3 seconds between starts to avoid a 100-client
// burst on otherwise fast authentication. This is provider-only configuration.
func parseStartupIntervalSeconds(value string) (time.Duration, error) {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 || seconds > 600 {
		return 0, fmt.Errorf("STARTUP_INTERVAL must be an integer from 0 to 600 seconds, got %q", value)
	}
	return time.Duration(seconds) * time.Second, nil
}

func providerStartupInterval(opts docopt.Opts) (time.Duration, error) {
	fromFlag, err := opts.String("--startup_interval")
	if err == nil && strings.TrimSpace(fromFlag) != "" {
		return parseStartupIntervalSeconds(fromFlag)
	}
	if fromEnvironment, exists := os.LookupEnv("STARTUP_INTERVAL"); exists {
		return parseStartupIntervalSeconds(fromEnvironment)
	}
	values, err := readProxyDotEnv(".env")
	if err != nil {
		return 0, err
	}
	if value, exists := values["STARTUP_INTERVAL"]; exists {
		return parseStartupIntervalSeconds(value)
	}
	return 3 * time.Second, nil
}

// Retries receive a deterministic per-slot delay to reduce synchronized
// reconnection attempts after an outage; the global pacer is applied again
// once this delay has elapsed. A 30-second backoff becomes 30-39 seconds.
func providerRetryDelay(index int) time.Duration {
	if index < 0 {
		index = 0
	}
	return 30*time.Second + time.Duration(index%10)*time.Second
}

func waitProviderRetry(ctx context.Context, delay time.Duration) error {
	if delay < 0 {
		return errors.New("negative retry delay")
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
