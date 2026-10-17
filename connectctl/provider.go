package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

type providerLimitError struct{ message string }
func (e *providerLimitError) Error() string { return e.message }

type providerSession struct {
	cancel context.CancelFunc
	strategy *connect.ClientStrategy
	api *connect.BringYourApi
	client *connect.Client
	transport *connect.PlatformTransport
	nat *connect.LocalUserNat
	provider *connect.RemoteUserNatProvider
	clientID connect.Id
	instanceID connect.Id
}

func (s *providerSession) Close() {
	s.cancel()
	s.transport.Close()
	s.provider.Close()
	s.nat.Close()
	s.client.Close()
	s.api.Close()
	s.strategy.Close()
}

// Each provider owns a distinct client JWT. The backend verifies credentials;
// unverified claims are used solely to read its server-issued client ID.
func authenticateProvider(ctx context.Context, api *connect.BringYourApi, known providerIdentity) (string, connect.Id, error) {
	request := &connect.AuthNetworkClientArgs{
		Description: "connectctl public provider",
		DeviceSpec: "",
	}
	if known.ClientID != "" {
		id, err := connect.ParseId(known.ClientID)
		if err != nil { return "", connect.Id{}, err }
		request.ClientId = &id
	}
	authCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	result, err := api.AuthNetworkClientSyncWithCtx(authCtx, request)
	if err != nil { return "", connect.Id{}, err }
	if result == nil { return "", connect.Id{}, errors.New("empty auth-client response") }
	if result.Error != nil {
		if result.Error.ClientLimitExceeded || result.Error.UpgradeRequired {
			return "", connect.Id{}, &providerLimitError{message: result.Error.Message}
		}
		return "", connect.Id{}, errors.New(result.Error.Message)
	}
	if result.ByClientJwt == "" { return "", connect.Id{}, errors.New("auth-client returned no JWT") }
	claims, err := connect.ParseByJwtUnverified(result.ByClientJwt)
	if err != nil || claims.ClientId == (connect.Id{}) {
		return "", connect.Id{}, errors.New("server returned an invalid client JWT")
	}
	if known.ClientID != "" && known.ClientID != claims.ClientId.String() {
		return "", connect.Id{}, errors.New("server re-authenticated a different client ID")
	}
	return result.ByClientJwt, claims.ClientId, nil
}

func startProvider(ctx context.Context, store *providerIdentities, index int, known providerIdentity,
	proxyConfig *connect.ProxySettings, networkJWT, apiURL, connectURL string) (_ *providerSession, err error) {
	ownerCtx, cancel := context.WithCancel(ctx)
	var strategy *connect.ClientStrategy
	if proxyConfig != nil {
		strategy, err = connect.NewProxyClientStrategy(ownerCtx, proxyConfig)
		if err != nil { cancel(); return nil, err }
	} else {
		strategy = connect.NewClientStrategyWithDefaults(ownerCtx)
	}
	api := connect.NewBringYourApi(ownerCtx, strategy, apiURL)
	api.SetByJwt(networkJWT)
	var client *connect.Client
	var nat *connect.LocalUserNat
	var provider *connect.RemoteUserNatProvider
	var transport *connect.PlatformTransport
	defer func() {
		if err != nil {
			cancel()
			if transport != nil { transport.Close() }
			if provider != nil { provider.Close() }
			if nat != nil { nat.Close() }
			if client != nil { client.Close() }
			api.Close()
			strategy.Close()
		}
	}()
	clientJWT, clientID, err := authenticateProvider(ownerCtx, api, known)
	if err != nil { return nil, err }
	if err = store.recordClient(index, known, clientID); err != nil { return nil, err }
	instanceID, err := connect.ParseId(known.InstanceID)
	if err != nil { return nil, err }
	api.SetByJwt(clientJWT)
	client = connect.NewClientWithDefaults(ownerCtx, clientID, connect.NewApiOutOfBandControlWithApi(api))
	// Match connectctl sink's event output. The callback borrows Frame data:
	// format it before returning, and never block the client's receive path.
	// The printer terminates when this provider session is canceled.
	attachProviderEventLogger(ownerCtx, client, index+1, clientID)
	natSettings := connect.DefaultLocalUserNatSettings()
	// TCP NAT obeys this session's selected proxy; UDP proxy egress is not
	// implemented. Other potential direct paths need separate validation.
	natSettings.TcpBufferSettings.ConnectSettings = *strategy.ConnectSettings()
	natSettings.UdpBufferSettings.ConnectSettings = *strategy.ConnectSettings()
	nat = connect.NewLocalUserNat(ownerCtx, clientID.String(), natSettings)
	provider, err = connect.TryNewRemoteUserNatProvider(client, nat, connect.DefaultRemoteUserNatProviderSettings())
	if err != nil { return nil, fmt.Errorf("start NAT provider: %w", err) }
	client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public: true,
		protocol.ProvideMode_Network: true,
	})
	auth := &connect.ClientAuth{
		ByJwt: clientJWT,
		InstanceId: instanceID,
		AppVersion: fmt.Sprintf("connectctl-provider %s", ConnectCtlVersion),
		ProvideIntent: true,
	}
	// TCP/H1 mode deliberately avoids direct QUIC or pinned family transports.
	settings := connect.DefaultPlatformTransportSettings()
	settings.AltUrl = ""
	transport = connect.NewPlatformTransportWithTargetMode(ownerCtx, strategy,
		client.RouteManager(), strings.TrimRight(connectURL, "/")+"/",
		auth, connect.TransportModeH1, settings)
	return &providerSession{
		cancel: cancel, strategy: strategy, api: api, client: client,
		nat: nat, provider: provider, transport: transport,
		clientID: clientID, instanceID: instanceID,
	}, nil
}

func providerDefaults(opts docopt.Opts) (proxyCLIConfig, error) {
	cfg, err := proxyOptions(opts)
	if err != nil { return cfg, err }
	if value, err := opts.String("--max_connection"); err != nil || value == "" {
		envValue, present := os.LookupEnv("MAX_CONNECTION")
		if !present || strings.TrimSpace(envValue) == "" {
			values, err := readProxyDotEnv(".env")
			if err != nil { return cfg, err }
			if strings.TrimSpace(values["MAX_CONNECTION"]) == "" { cfg.maxConnection = 1 }
		}
	}
	if cfg.maxConnection > 100 {
		return cfg, errors.New("MAX_CONNECTION for provider must be <= 100; account limits may be lower")
	}
	if cfg.concurrency > cfg.maxConnection { cfg.concurrency = cfg.maxConnection }
	return cfg, nil
}

func provide(opts docopt.Opts) {
	cfg, err := providerDefaults(opts)
	if err != nil { Err.Printf("provider options: %v", err); return }
	statsInterval, err := providerStatsInterval(opts)
	if err != nil { Err.Printf("provider stats interval: %v", err); return }
	apiURL, err := opts.String("--api_url")
	if err != nil || apiURL == "" { apiURL = DefaultApiUrl }
	connectURL, err := opts.String("--connect_url")
	if err != nil || connectURL == "" { connectURL = DefaultConnectUrl }
	rawJWT, err := os.ReadFile(filepath.Join(".urnetwork", "jwt"))
	if err != nil {
		Err.Printf("network JWT not found: run connectctl auth first (%v)", err)
		return
	}
	networkJWT := strings.TrimSpace(string(rawJWT))
	claims, err := connect.ParseByJwtUnverified(networkJWT)
	if err != nil || claims.ClientId != (connect.Id{}) {
		Err.Print("expected a network JWT in .urnetwork/jwt, not a client JWT")
		return
	}
	store, err := openProviderIdentities(defaultProviderIdentityPath())
	if err != nil { Err.Printf("provider state: %v", err); return }
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var pool *connect.ProxyPool
	if cfg.enabled() {
		loadCtx, stop := context.WithTimeout(ctx, 90*time.Second)
		pool, err = loadCLIProxyPool(loadCtx, cfg)
		stop()
		if err != nil { Err.Printf("proxy loading: %v", err); return }
	}
	totalProxies := 0
	if pool != nil { totalProxies = pool.Len() }
	Out.Printf("[provider] MAX_CONNECTION=%d CONCURRENCY=%d PROXIES=%d", cfg.maxConnection, cfg.concurrency, totalProxies)
	sem := make(chan struct{}, cfg.concurrency)
	metrics := make([]*providerLocalMetrics, cfg.maxConnection)
	for i := range metrics {
		metrics[i] = newProviderLocalMetrics(time.Now())
	}
	runProviderStatsReporter(ctx, statsInterval, apiURL, networkJWT, pool, store, metrics)
	if statsInterval > 0 {
		Out.Printf("[stats] local and official 24h server summary every %s (set --stats_interval=0 to disable)", statsInterval)
	}
	var workers sync.WaitGroup
	for i := 0; i < cfg.maxConnection; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for ctx.Err() == nil {
				known, err := store.loadOrCreate(index)
				if err != nil { Err.Printf("[provider %d] invalid state: %v", index+1, err); return }
				var chosen *connect.ProxySettings
				if pool != nil { chosen = pool.Next() }
				select {
				case sem <- struct{}{}:
				case <-ctx.Done(): return
				}
				session, err := startProvider(ctx, store, index, known, chosen, networkJWT, apiURL, connectURL)
				<-sem
				if err != nil {
					metrics[index].startupFailed()
					var limit *providerLimitError
					if errors.As(err, &limit) {
						Err.Printf("[provider %d] server client limit: %v", index+1, err)
						return
					}
					Err.Printf("[provider %d] startup failed: %v; retrying in 30s", index+1, err)
					select {
					case <-ctx.Done(): return
					case <-time.After(30*time.Second): continue
					}
				}
				route := "direct"
				if chosen != nil { route = fmt.Sprintf("%s://%s", chosen.Protocol, chosen.Address) }
				Out.Printf("[provider %d] registered client_id=%s instance_id=%s via %s (awaiting connection)",
					index+1, session.clientID, session.instanceID, route)
				lastSuccess := time.Now()
				ticker := time.NewTicker(5 * time.Second)
				retry := false
				for !retry {
					connected := session.transport.IsConnected()
					if connected {
						lastSuccess = time.Now()
					}
					// Count route registrations and disconnects separately.
					// These are not the internal SDK dialer success/error totals.
					if metrics[index].observe(time.Now(), connected) {
						snap := metrics[index].snapshot(time.Now())
						Out.Printf("[provider %d] connected=%t connect_ok=%d init_errors=%d disconnects=%d",
							index+1, connected, snap.ConnectOK, snap.StartupErrors, snap.Disconnects)
					}
					select {
					case <-ctx.Done():
						ticker.Stop()
						metrics[index].sessionEnded(time.Now())
						session.Close()
						return
					case <-ticker.C:
						// One provider uses a stable proxy during its session.
						// Rotation on prolonged outage never creates a new client ID.
						if pool != nil && pool.Len() > 1 && !connected && time.Since(lastSuccess) >= 90*time.Second {
							Err.Printf("[provider %d] offline >90s; trying next proxy", index+1)
							retry = true
						}
					}
				}
				ticker.Stop()
				metrics[index].sessionEnded(time.Now())
				session.Close()
			}
		}(i)
	}
	workers.Wait()
}


// attachProviderEventLogger prints the same network-control frame summary as
// connectctl sink, with the provider index and client_id included so event
// streams from distinct provider clients can be distinguished.
//
// Payload/data-plane frames are never logged. Messages are not retained:
// callback-provided frames may be recycled when
// the callback returns. Logging is bounded, and a slow console cannot stall
// the network receive pump. Not every provider receives NetworkPeersUpdate:
// the backend controls which events a public provider is allowed to see.
func attachProviderEventLogger(ctx context.Context, client *connect.Client, providerIndex int, clientID connect.Id) {
	const bufferSize = 256
	events := make(chan *sinkReceive, bufferSize)
	var dropped atomic.Uint64

	client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
		if ctx.Err() != nil {
			return
		}
		snapshot := snapshotProviderNetworkEvent(source, frames, peer)
		if snapshot == nil {
			return
		}
		if !enqueueSinkReceive(events, snapshot) {
			dropped.Add(1)
		}
	})

	go func() {
		// Event lines are emitted only on receipt, never on a timer.
		// Drop reports are compact and throttled.
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				Out.Printf("[provider %d client=%s] [%s %s] %s",
					providerIndex, clientID, event.source, event.provideMode, event.frameSummary)
			case <-ticker.C:
				if count := dropped.Swap(0); count > 0 {
					Err.Printf("[provider %d] dropped %d received event log entries (console is slower than network)",
						providerIndex, count)
				}
			}
		}
	}()
}

// snapshotProviderNetworkEvent formats the network-control frames shown by
// connectctl sink (peer resets/updates and session changes). Provider traffic
// can include third-party payloads: never dump data-plane frames to logs.
func snapshotProviderNetworkEvent(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) *sinkReceive {
	var controlFrames []*protocol.Frame
	for _, frame := range frames {
		if frame == nil {
			continue
		}
		switch frame.GetMessageType() {
		case protocol.MessageType_TransferNetworkPeersReset,
			protocol.MessageType_TransferNetworkPeersUpdate,
			protocol.MessageType_TransferNetworkSessionsChanged:
			controlFrames = append(controlFrames, frame)
		}
	}
	if len(controlFrames) == 0 {
		return nil
	}
	return snapshotSinkReceive(source, controlFrames, peer)
}
