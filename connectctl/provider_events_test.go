package main

import (
	"sync"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect/protocol"
)

func TestProviderEventModeParse(t *testing.T) {
	for _, tc := range []struct{ input string; expected providerEventLogMode; valid bool }{
		{"off", providerEventLogOff, true},
		{"OFF", providerEventLogOff, true},
		{" summary ", providerEventLogSummary, true},
		{"full", providerEventLogFull, true},
		{"", "", false},
		{"verbose", "", false},
		{"0", "", false},
	} {
		actual, err := parseProviderEventLogMode(tc.input)
		if (err == nil) != tc.valid || (tc.valid && actual != tc.expected) {
			t.Errorf("parse %q: mode %q, error %v", tc.input, actual, err)
		}
	}
}

func TestProviderEventModeFlagOverridesEnv(t *testing.T) {
	t.Setenv("EVENT_LOG", "summary")
	args, err := docopt.ParseArgs(connectCtlUsage(), []string{
		"provide", "--event_log=full", "--max_connection=2",
	}, ConnectCtlVersion)
	if err != nil { t.Fatal(err) }
	mode, err := providerEventLogSetting(args)
	if err != nil || mode != providerEventLogFull {
		t.Fatalf("flag did not override env: mode=%s err=%v", mode, err)
	}
	envArgs, err := docopt.ParseArgs(connectCtlUsage(), []string{"provide"}, ConnectCtlVersion)
	if err != nil { t.Fatal(err) }
	mode, err = providerEventLogSetting(envArgs)
	if err != nil || mode != providerEventLogSummary {
		t.Fatalf("env not used: mode=%s err=%v", mode, err)
	}
}

func TestProviderEventCountersAggregateWithoutLoggingPayload(t *testing.T) {
	counters := &providerEventCounters{}
	const goroutines = 8
	const each = 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				counters.add(protocol.MessageType_TransferNetworkPeersReset)
				counters.add(protocol.MessageType_TransferNetworkPeersUpdate)
				counters.add(protocol.MessageType_TransferNetworkSessionsChanged)
				counters.add(protocol.MessageType_IpIpPacketFromProvider)
			}
		}()
	}
	wg.Wait()
	got := counters.swap()
	want := uint64(goroutines * each)
	if got.PeersReset != want || got.PeersUpdate != want ||
		got.SessionsChanged != want || got.total() != 3*want {
		t.Fatalf("wrong event counts: %+v", got)
	}
	if got := counters.swap(); got.total() != 0 {
		t.Fatalf("counters not reset after summary: %+v", got)
	}
}

func TestProviderEventCLIUsage(t *testing.T) {
	for _, args := range [][]string{
		{"provide", "--event_log=off"},
		{"provide", "--event_log=summary", "--stats_interval=300"},
		{"provide", "--event_log=full"},
	} {
		if _, err := docopt.ParseArgs(connectCtlUsage(), args, ConnectCtlVersion); err != nil {
			t.Errorf("parse %v: %v", args, err)
		}
	}
}
