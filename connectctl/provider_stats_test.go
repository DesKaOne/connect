package main

import (
	"context"
	"errors"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

func TestOfficialProviderStatsDecodesUnitsAndFiltersLocalSlots(t *testing.T) {
	body := []byte(`{"created_time":"2026-10-17T12:00:00Z","providers":[
		{"client_id":"client-a","connected":true,"uptime_last_24h":18.5,"transfer_data_last_24h":0.25,
		"payout_last_24h":0.012,"contracts_last_24h":7,"clients_last_24h":3},
		{"client_id":"client-b","connected":false,"uptime_last_24h":1,"transfer_data_last_24h":0.01,
		"payout_last_24h":0,"contracts_last_24h":0,"clients_last_24h":0}
	]}`)
	var data serverProvidersResult
	if err := json.Unmarshal(body, &data); err != nil { t.Fatal(err) }
	if len(data.Providers) != 2 { t.Fatal("unexpected provider count") }
	if data.Providers[0].TransferDataLast24h != 0.25 || data.Providers[0].UptimeLast24h != 18.5 {
		t.Fatal("unexpected numeric units or values")
	}

	var output bytes.Buffer
	count := printServerProviderStats(&output, &data, map[string]int{"client-a": 2})
	if count != 1 || !strings.Contains(output.String(), "[stats provider 2]") {
		t.Fatalf("expected only local provider 2, got %q", output.String())
	}
	if !strings.Contains(output.String(), "18.50h") || !strings.Contains(output.String(), "0.2500GiB") ||
		!strings.Contains(output.String(), "0.012000USDC") || strings.Contains(output.String(), "client-b") {
		t.Fatalf("bad stats text: %q", output.String())
	}
}

func TestProviderLocalMetricsCountsTransitionsAndTime(t *testing.T) {
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m := newProviderLocalMetrics(start)
	if !m.observe(start, false) { t.Fatal("initial disconnected state must log once") }
	if m.observe(start.Add(time.Second), false) { t.Fatal("repeated false is not a transition") }
	m.startupFailed()
	if !m.observe(start.Add(2*time.Second), true) { t.Fatal("first connection not observed") }
	if m.observe(start.Add(3*time.Second), true) { t.Fatal("repeated true is not a transition") }
	if !m.observe(start.Add(12*time.Second), false) { t.Fatal("disconnect not observed") }
	if !m.observe(start.Add(14*time.Second), true) { t.Fatal("reconnect not observed") }
	snap := m.snapshot(start.Add(18*time.Second))
	if !snap.Connected || snap.ConnectOK != 2 || snap.StartupErrors != 1 || snap.Disconnects != 1 {
		t.Fatalf("unexpected metrics: %+v", snap)
	}
	if snap.ConnectedTime != 14*time.Second {
		t.Fatalf("connected time=%s, want 14s", snap.ConnectedTime)
	}
	m.sessionEnded(start.Add(19*time.Second))
	snap = m.snapshot(start.Add(20*time.Second))
	if snap.Connected || snap.Disconnects != 2 || snap.ConnectedTime != 15*time.Second {
		t.Fatalf("session shutdown metrics wrong: %+v", snap)
	}
}

func TestProviderStatsCLIOptions(t *testing.T) {
	for _, args := range [][]string{
		{"stats"},
		{"stats", "--proxy_list_txt=proxy.txt"},
		{"provide", "--stats_interval=0"},
		{"provide", "--stats_interval=30", "--max_connection=2"},
	} {
		if _, err := docopt.ParseArgs(connectCtlUsage(), args, ConnectCtlVersion); err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
	}
	for _, tc := range []struct{ input string; seconds int; valid bool }{
		{"0", 0, true}, {"30", 30, true}, {"60", 60, true},
		{"29", 0, false}, {"-1", 0, false}, {"abc", 0, false}, {"3601", 0, false},
	} {
		opts := docopt.Opts{"--stats_interval": tc.input}
		got, err := providerStatsInterval(opts)
		if (err == nil) != tc.valid {
			t.Errorf("%q: error=%v valid=%t", tc.input, err, tc.valid)
		}
		if tc.valid && got != time.Duration(tc.seconds)*time.Second {
			t.Errorf("%q: duration %v", tc.input, got)
		}
	}
}

func TestProxyPoolFirstDoesNotShiftNext(t *testing.T) {
	a := &connect.ProxySettings{Protocol: connect.ProxyProtocolSOCKS5, Address: "localhost:1080"}
	b := &connect.ProxySettings{Protocol: connect.ProxyProtocolHTTP, Address: "localhost:8080"}
	pool, err := connect.NewProxyPool([]*connect.ProxySettings{a, b})
	if err != nil { t.Fatal(err) }
	first := pool.First()
	first.Address = "example.invalid:9"
	if next := pool.Next(); next.Address != a.Address {
		t.Errorf("stats strategy changed provider pool rotation: got %s", next.Address)
	}
	if next := pool.Next(); next.Address != b.Address {
		t.Errorf("second provider: got %s", next.Address)
	}
}


func TestProviderStatsFallbackAfterServerCacheFailure(t *testing.T) {
	ctx := context.Background()
	postCalls := 0
	stats, err := fetchProviderStatsWithFallback(ctx,
		func(context.Context) ([]byte, error) {
			return nil, &connect.HttpStatusError{
				StatusCode: 500,
				Status: "500 Internal Server Error",
				Body: []byte("503 Cached response is temporarily unavailable."),
			}
		},
		func(context.Context) ([]byte, error) {
			postCalls++
			return []byte(`{"created_time":"2026-10-17T00:00:00Z","providers":[{"client_id":"one","connected":true}]}`), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if postCalls != 1 || stats.SourceEndpoint != statsProvidersLastNEndpoint ||
		len(stats.Providers) != 1 || stats.Providers[0].ClientID != "one" {
		t.Fatalf("unexpected fallback: count=%d data=%+v", postCalls, stats)
	}
}

func TestProviderStatsNoFallbackOnAuthorizationFailure(t *testing.T) {
	postCalls := 0
	_, err := fetchProviderStatsWithFallback(context.Background(),
		func(context.Context) ([]byte, error) {
			return nil, &connect.HttpStatusError{
				StatusCode: 403,
				Status: "403 Forbidden",
				Body: []byte("Not authorized"),
			}
		},
		func(context.Context) ([]byte, error) {
			postCalls++
			return nil, nil
		},
	)
	if err == nil || postCalls != 0 {
		t.Fatalf("must not retry on unauthorized GET: err=%v postCalls=%d", err, postCalls)
	}
}

func TestProviderStatsReportsBothServerFailures(t *testing.T) {
	_, err := fetchProviderStatsWithFallback(context.Background(),
		func(context.Context) ([]byte, error) {
			return nil, &connect.HttpStatusError{StatusCode: 500, Status: "500 Server Error"}
		},
		func(context.Context) ([]byte, error) {
			return nil, &connect.HttpStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
		},
	)
	if err == nil ||
		!strings.Contains(err.Error(), statsProvidersEndpoint) ||
		!strings.Contains(err.Error(), statsProvidersLastNEndpoint) ||
		!strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("expected actionable error containing both endpoint failures; got %v", err)
	}
}

func TestProviderStatsHealthyGETDoesNotUseFallback(t *testing.T) {
	postCalls := 0
	stats, err := fetchProviderStatsWithFallback(context.Background(),
		func(context.Context) ([]byte, error) {
			return []byte(`{"created_time":"2026-10-17T00:00:00Z","providers":[]}`), nil
		},
		func(context.Context) ([]byte, error) {
			postCalls++
			return nil, errors.New("should not call fallback")
		},
	)
	if err != nil || postCalls != 0 || stats.SourceEndpoint != statsProvidersEndpoint {
		t.Fatalf("healthy GET should succeed directly: err=%v postCalls=%d stats=%+v", err, postCalls, stats)
	}
}
