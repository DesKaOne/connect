package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

func TestProviderCommandsParse(t *testing.T) {
	cases := [][]string{
		{"auth"},
		{"auth", "--api_url=https://api.bringyour.com"},
		{"provide"},
		{"provide", "--max_connection=2", "--concurrency=2"},
		{"provide", "--proxy_list_txt=proxy.txt", "--max_connection=3", "--concurrency=2"},
		{"provide", "--proxy_hub_url=http://127.0.0.1:8080"},
	}
	for _, args := range cases {
		if _, err := docopt.ParseArgs(connectCtlUsage(), args, ConnectCtlVersion); err != nil {
			t.Errorf("parse %v: %v", args, err)
		}
	}
}

func TestProviderIdentityDistinctAndPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	store, err := openProviderIdentities(path)
	if err != nil { t.Fatal(err) }
	first, err := store.loadOrCreate(0)
	if err != nil { t.Fatal(err) }
	second, err := store.loadOrCreate(1)
	if err != nil { t.Fatal(err) }
	if first.InstanceID == second.InstanceID {
		t.Fatal("provider slots share the same instance ID")
	}
	clientOne, clientTwo := connect.NewId(), connect.NewId()
	if err := store.recordClient(0, first, clientOne); err != nil { t.Fatal(err) }
	if err := store.recordClient(1, second, clientTwo); err != nil { t.Fatal(err) }
	if err := store.recordClient(0, first, clientTwo); err == nil {
		t.Fatal("different client ID silently replaced existing provider identity")
	}
	reloaded, err := openProviderIdentities(path)
	if err != nil { t.Fatal(err) }
	one, err := reloaded.loadOrCreate(0)
	if err != nil { t.Fatal(err) }
	two, err := reloaded.loadOrCreate(1)
	if err != nil { t.Fatal(err) }
	if one.ClientID != clientOne.String() || two.ClientID != clientTwo.String() ||
		one.InstanceID != first.InstanceID || two.InstanceID != second.InstanceID {
		t.Fatalf("persisted IDs mismatched: %#v %#v", one, two)
	}
}

func TestProviderLimitFlags(t *testing.T) {
	t.Setenv("MAX_CONNECTION", "100")
	t.Setenv("CONCURRENCY", "10")
	for _, tc := range []struct {
		args []string
		wantMax int
		wantConcurrency int
	}{
		{[]string{"provide"}, 100, 10},
		{[]string{"provide", "--max_connection=2", "--concurrency=1"}, 2, 1},
	} {
		args, err := docopt.ParseArgs(connectCtlUsage(), tc.args, ConnectCtlVersion)
		if err != nil { t.Fatal(err) }
		cfg, err := providerDefaults(args)
		if err != nil { t.Fatal(err) }
		if cfg.maxConnection != tc.wantMax || cfg.concurrency != tc.wantConcurrency {
			t.Errorf("args %v: got max=%d concurrency=%d", tc.args, cfg.maxConnection, cfg.concurrency)
		}
	}
}


func TestSnapshotProviderNetworkEventFiltersPayloads(t *testing.T) {
	source := connect.SourceId(connect.NewId())
	peer := connect.Peer{ProvideMode: protocol.ProvideMode_Network}
	frames := []*protocol.Frame{
		{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: []byte("PRIVATE-DATA")},
		{MessageType: protocol.MessageType_TransferNetworkPeersUpdate, MessageBytes: []byte("peers")},
		{MessageType: protocol.MessageType_TransferNetworkSessionsChanged, MessageBytes: []byte("sessions")},
	}
	snapshot := snapshotProviderNetworkEvent(source, frames, peer)
	if snapshot == nil {
		t.Fatal("control event was filtered out")
	}
	if strings.Contains(snapshot.frameSummary, "PRIVATE-DATA") || strings.Contains(snapshot.frameSummary, "IpIpPacketFromProvider") {
		t.Fatalf("provider log exposed data-plane frame: %s", snapshot.frameSummary)
	}
	if !strings.Contains(snapshot.frameSummary, "TransferNetworkPeersUpdate") || !strings.Contains(snapshot.frameSummary, "TransferNetworkSessionsChanged") {
		t.Fatalf("network control event missing: %s", snapshot.frameSummary)
	}

	// Borrowed frame bytes may be reused after the callback returns.
	frames[1].MessageBytes = []byte("changed")
	if strings.Contains(snapshot.frameSummary, "changed") {
		t.Fatal("snapshot retained the borrowed frame data")
	}
	if filtered := snapshotProviderNetworkEvent(source, frames[:1], peer); filtered != nil {
		t.Fatalf("data-only event should not be printed: %+v", filtered)
	}
}
