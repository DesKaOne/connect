package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

type providerEventLogMode string

const (
	providerEventLogOff     providerEventLogMode = "off"
	providerEventLogSummary providerEventLogMode = "summary"
	providerEventLogFull    providerEventLogMode = "full"
)

func parseProviderEventLogMode(value string) (providerEventLogMode, error) {
	switch providerEventLogMode(strings.ToLower(strings.TrimSpace(value))) {
	case providerEventLogOff:
		return providerEventLogOff, nil
	case providerEventLogSummary:
		return providerEventLogSummary, nil
	case providerEventLogFull:
		return providerEventLogFull, nil
	default:
		return "", fmt.Errorf("EVENT_LOG must be off, summary, or full (got %q)", value)
	}
}

// CLI > process environment > .env > default. This affects provide only;
// sink retains its existing received-message logging.
func providerEventLogSetting(opts docopt.Opts) (providerEventLogMode, error) {
	if value, err := opts.String("--event_log"); err == nil && strings.TrimSpace(value) != "" {
		return parseProviderEventLogMode(value)
	}
	if value, ok := os.LookupEnv("EVENT_LOG"); ok {
		return parseProviderEventLogMode(value)
	}
	dotenv, err := readProxyDotEnv(".env")
	if err != nil {
		return "", err
	}
	if value, ok := dotenv["EVENT_LOG"]; ok {
		return parseProviderEventLogMode(value)
	}
	return providerEventLogOff, nil
}

// Counts are aggregated across ALL provider sessions without retaining
// message contents, client IDs, or payload bytes.
type providerEventCounters struct {
	peersReset atomic.Uint64
	peersUpdate atomic.Uint64
	sessionsChanged atomic.Uint64
}

type providerEventSnapshot struct {
	PeersReset uint64
	PeersUpdate uint64
	SessionsChanged uint64
}

func (c *providerEventCounters) add(messageType protocol.MessageType) {
	switch messageType {
	case protocol.MessageType_TransferNetworkPeersReset:
		c.peersReset.Add(1)
	case protocol.MessageType_TransferNetworkPeersUpdate:
		c.peersUpdate.Add(1)
	case protocol.MessageType_TransferNetworkSessionsChanged:
		c.sessionsChanged.Add(1)
	}
}

func (c *providerEventCounters) swap() providerEventSnapshot {
	return providerEventSnapshot{
		PeersReset: c.peersReset.Swap(0),
		PeersUpdate: c.peersUpdate.Swap(0),
		SessionsChanged: c.sessionsChanged.Swap(0),
	}
}

func (s providerEventSnapshot) total() uint64 {
	return s.PeersReset + s.PeersUpdate + s.SessionsChanged
}

// Summary mode has a single 60-second ticker for the entire process,
// never one ticker per provider. It doesn't make network/API calls.
func runProviderEventSummary(ctx context.Context, counters *providerEventCounters) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				counts := counters.swap()
				if counts.total() > 0 {
					Out.Printf("[events 60s] peers_reset=%d peers_update=%d sessions_changed=%d total=%d",
						counts.PeersReset, counts.PeersUpdate, counts.SessionsChanged, counts.total())
				}
			}
		}
	}()
}

// Never serialize or log payloads in summary mode. Counter operations are
// O(number of network-control frames) and don't block the receive callback.
func attachProviderEventSummary(ctx context.Context, client *connect.Client, counters *providerEventCounters) {
	client.AddReceiveCallback(func(_ connect.TransferPath, frames []*protocol.Frame, _ connect.Peer) {
		if ctx.Err() != nil {
			return
		}
		for _, frame := range frames {
			if frame != nil {
				counters.add(frame.GetMessageType())
			}
		}
	})
}
