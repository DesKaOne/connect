package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// The official API gives uptime (hours), transfer (GiB), payout (USDC).
// These are server-side 24h measurements, not local byte estimates.
type serverProviderStats struct {
	ClientID             string  `json:"client_id"`
	Connected            bool    `json:"connected"`
	UptimeLast24h        float64 `json:"uptime_last_24h"`
	TransferDataLast24h  float64 `json:"transfer_data_last_24h"`
	PayoutLast24h        float64 `json:"payout_last_24h"`
	ContractsLast24h     int64   `json:"contracts_last_24h"`
	ClientsLast24h       int64   `json:"clients_last_24h"`
}
type serverProvidersResult struct {
	CreatedTime string                `json:"created_time"`
	Providers   []serverProviderStats `json:"providers"`
	SourceEndpoint string `json:"-"`
}

const (
	statsProvidersEndpoint = "/stats/providers"
	statsProvidersLastNEndpoint = "/stats/providers-last-n"
)

func decodeServerProviderStats(body []byte, endpoint string) (*serverProvidersResult, error) {
	var result serverProvidersResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode %s: %w", endpoint, err)
	}
	if result.Providers == nil {
		return nil, fmt.Errorf("%s returned no providers array", endpoint)
	}
	result.SourceEndpoint = endpoint
	return &result, nil
}

// A 5xx error can mean the server's cached statistics are temporarily
// unavailable. Try the officially documented last-N-hours endpoint once.
// Do not fallback on 401/403, invalid credentials or other client errors.
// In particular, never replace a failed response with fabricated zeroes.
func fetchProviderStatsWithFallback(
	ctx context.Context,
	get func(context.Context) ([]byte, error),
	post func(context.Context) ([]byte, error),
) (*serverProvidersResult, error) {
	body, err := get(ctx)
	if err == nil {
		return decodeServerProviderStats(body, statsProvidersEndpoint)
	}

	var statusErr *connect.HttpStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode < 500 || statusErr.StatusCode >= 600 {
		return nil, fmt.Errorf("GET %s: %w", statsProvidersEndpoint, err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	body, postErr := post(ctx)
	if postErr != nil {
		return nil, fmt.Errorf(
			"URnetwork server statistics unavailable (provider connections are unaffected by this result): GET %s: %v; fallback POST %s: %w",
			statsProvidersEndpoint, err, statsProvidersLastNEndpoint, postErr,
		)
	}
	return decodeServerProviderStats(body, statsProvidersLastNEndpoint)
}

func fetchServerProviderStats(ctx context.Context, strategy *connect.ClientStrategy, apiURL, networkJWT string) (*serverProvidersResult, error) {
	if strings.TrimSpace(networkJWT) == "" {
		return nil, errors.New("missing network JWT for provider stats")
	}
	base := strings.TrimRight(apiURL, "/")
	return fetchProviderStatsWithFallback(ctx,
		func(requestCtx context.Context) ([]byte, error) {
			return connect.HttpGetWithStrategyRaw(requestCtx, strategy,
				base+statsProvidersEndpoint, networkJWT)
		},
		func(requestCtx context.Context) ([]byte, error) {
			// This POST response has the same StatsProvidersResult schema as GET.
			return connect.HttpPostWithStrategyRaw(requestCtx, strategy,
				base+statsProvidersLastNEndpoint, []byte(`{"last_n":24}`), networkJWT)
		},
	)
}

func printServerProviderStats(w io.Writer, stats *serverProvidersResult, localIDs map[string]int) (shown int) {
	if stats == nil { return 0 }
	ordered := append([]serverProviderStats(nil), stats.Providers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ClientID < ordered[j].ClientID })
	for _, entry := range ordered {
		index := 0
		if localIDs != nil {
			index = localIDs[entry.ClientID]
			if index == 0 { continue }
		}
		online := "offline"
		if entry.Connected { online = "online" }
		tag := "network"
		if index != 0 { tag = fmt.Sprintf("provider %d", index) }
		fmt.Fprintf(w,
			"[stats %s] client=%s server=%s uptime_24h=%.2fh transfer_24h=%.4fGiB payout_24h=%.6fUSDC contracts=%d clients=%d\n",
			tag, entry.ClientID, online, entry.UptimeLast24h, entry.TransferDataLast24h,
			entry.PayoutLast24h, entry.ContractsLast24h, entry.ClientsLast24h)
		shown++
	}
	return shown
}

func loadNetworkJWT() (string, error) {
	contents, err := os.ReadFile(filepath.Join(".urnetwork", "jwt"))
	if err != nil {
		return "", fmt.Errorf("missing .urnetwork/jwt (run connectctl auth): %w", err)
	}
	jwt := strings.TrimSpace(string(contents))
	if jwt == "" { return "", errors.New(".urnetwork/jwt is empty") }
	claims, err := connect.ParseByJwtUnverified(jwt)
	if err != nil || claims.ClientId != (connect.Id{}) {
		return "", errors.New("provider stats requires a network JWT, not a Client JWT")
	}
	return jwt, nil
}

func newStatsStrategy(ctx context.Context, pool *connect.ProxyPool) (*connect.ClientStrategy, error) {
	if pool == nil { return connect.NewClientStrategyWithDefaults(ctx), nil }
	return connect.NewProxyClientStrategy(ctx, pool.First())
}

// stats is an on-demand API query that does not create any provider clients.
// It uses the network JWT saved by the local auth command.
func stats(opts docopt.Opts) {
	networkJWT, err := loadNetworkJWT()
	if err != nil { Err.Printf("stats: %v", err); return }
	apiURL, err := opts.String("--api_url")
	if err != nil || apiURL == "" { apiURL = DefaultApiUrl }
	cfg, err := proxyOptions(opts)
	if err != nil { Err.Printf("stats options: %v", err); return }
	ctx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	defer stop()
	var pool *connect.ProxyPool
	if cfg.enabled() {
		pool, err = loadCLIProxyPool(ctx, cfg)
		if err != nil { Err.Printf("stats proxies: %v", err); return }
	}
	strategy, err := newStatsStrategy(ctx, pool)
	if err != nil { Err.Printf("stats strategy: %v", err); return }
	defer strategy.Close()
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := fetchServerProviderStats(queryCtx, strategy, apiURL, networkJWT)
	if err != nil { Err.Printf("stats API: %v", err); return }
	Out.Printf("[stats] source=URnetwork API %s window=24h sampled_at=%s providers=%d",
		result.SourceEndpoint, result.CreatedTime, len(result.Providers))
	printServerProviderStats(os.Stdout, result, nil)
}

type providerLocalMetrics struct {
	mu             sync.Mutex
	startedAt      time.Time
	statusSeen     bool
	connected      bool
	connectedSince time.Time
	connectedTotal time.Duration
	connectOK      uint64
	startupErrors  uint64
	disconnects    uint64
}

type providerLocalSnapshot struct {
	Connected     bool
	ConnectOK     uint64
	StartupErrors uint64
	Disconnects   uint64
	ConnectedTime time.Duration
	Elapsed       time.Duration
}

func newProviderLocalMetrics(now time.Time) *providerLocalMetrics {
	return &providerLocalMetrics{startedAt: now}
}

func (m *providerLocalMetrics) startupFailed() {
	m.mu.Lock()
	m.startupErrors++
	m.mu.Unlock()
}

// Count first successful route registration, but do not mistake initial
// connected=false for a failed dial attempt.
func (m *providerLocalMetrics) observe(now time.Time, connected bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := !m.statusSeen || connected != m.connected
	if m.statusSeen && m.connected && !connected {
		m.disconnects++
		m.connectedTotal += now.Sub(m.connectedSince)
		m.connectedSince = time.Time{}
	}
	if connected && (!m.statusSeen || !m.connected) {
		m.connectOK++
		m.connectedSince = now
	}
	m.statusSeen = true
	m.connected = connected
	return changed
}

func (m *providerLocalMetrics) snapshot(now time.Time) providerLocalSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	up := m.connectedTotal
	if m.connected { up += now.Sub(m.connectedSince) }
	return providerLocalSnapshot{
		Connected: m.connected, ConnectOK: m.connectOK,
		StartupErrors: m.startupErrors, Disconnects: m.disconnects,
		ConnectedTime: up, Elapsed: now.Sub(m.startedAt),
	}
}

func (m *providerLocalMetrics) sessionEnded(now time.Time) {
	m.observe(now, false)
}

// One API request per interval for all locally managed providers.
// In proxy mode the stats strategy also uses a proxy (never a direct fallback).
func runProviderStatsReporter(ctx context.Context, interval time.Duration, apiURL, networkJWT string,
	pool *connect.ProxyPool, store *providerIdentities, metrics []*providerLocalMetrics) {
	if interval == 0 { return }
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		strategy, err := newStatsStrategy(ctx, pool)
		if err != nil { Err.Printf("[stats] strategy initialization error: %v", err); return }
		defer strategy.Close()
		for {
			select {
			case <-ctx.Done(): return
			case <-ticker.C:
			}
			now := time.Now()
			online := 0
			for i, entry := range metrics {
				snap := entry.snapshot(now)
				if snap.Connected { online++ }
				Out.Printf("[local %d] connected=%t connect_ok=%d init_errors=%d disconnects=%d online=%s runtime=%s",
					i+1, snap.Connected, snap.ConnectOK, snap.StartupErrors, snap.Disconnects,
					snap.ConnectedTime.Truncate(time.Second), snap.Elapsed.Truncate(time.Second))
			}
			Out.Printf("[local] connected=%d/%d (since process start; not 24h server stats)", online, len(metrics))
			queryCtx, stop := context.WithTimeout(ctx, 25*time.Second)
			data, err := fetchServerProviderStats(queryCtx, strategy, apiURL, networkJWT)
			stop()
			if err != nil {
				Err.Printf("[stats] URnetwork API request failed: %v (local counters still available)", err)
				continue
			}
			store.mu.Lock()
			ids := make(map[string]int, len(store.records))
			for index, record := range store.records {
				if record.ClientID == "" { continue }
				n, err := strconv.Atoi(index)
				if err == nil && n >= 0 && n < len(metrics) { ids[record.ClientID] = n+1 }
			}
			store.mu.Unlock()
			Out.Printf("[stats] source=URnetwork API %s window=24h sampled_at=%s", data.SourceEndpoint, data.CreatedTime)
			if count := printServerProviderStats(os.Stdout, data, ids); count == 0 {
				Out.Print("[stats] no managed provider IDs in current API response yet")
			}
		}
	}()
}

func providerStatsInterval(opts docopt.Opts) (time.Duration, error) {
	value, err := opts.String("--stats_interval")
	if err != nil || strings.TrimSpace(value) == "" { return time.Minute, nil }
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 || (seconds > 0 && seconds < 30) || seconds > 3600 {
		return 0, errors.New("--stats_interval must be 0 (off) or 30..3600 seconds")
	}
	return time.Duration(seconds)*time.Second, nil
}
