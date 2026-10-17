package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// proxyCLIConfig configures the optional CLI proxy mode. CLI flags override
// process environment, which overrides the .env file in the working dir.
// Without proxy sources the original direct send/sink mode is unchanged.
type proxyCLIConfig struct {
	maxConnection int
	concurrency   int
	proxyHubURL   string
	proxyListTXT  string
}

func readProxyDotEnv(path string) (map[string]string, error) {
	values := map[string]string{}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export "))
		switch key {
		case "MAX_CONNECTION", "CONCURRENCY", "PROXY_HUB_URL", "PROXY_LIST_TXT":
		default:
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf(".env %s: %w", key, err)
			}
			value = unquoted
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		values[key] = value
	}
	return values, scanner.Err()
}

func readProxyCLIConfig(opts docopt.Opts, dotenv map[string]string) (proxyCLIConfig, error) {
	get := func(flag, env string) string {
		if value, err := opts.String(flag); err == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		if value, exists := os.LookupEnv(env); exists {
			return strings.TrimSpace(value)
		}
		return strings.TrimSpace(dotenv[env])
	}
	parsePositive := func(flag, env string, fallback int) (int, error) {
		value := get(flag, env)
		if value == "" {
			return fallback, nil
		}
		count, err := strconv.Atoi(value)
		if err != nil || count < 1 || count > 1000 {
			return 0, fmt.Errorf("%s must be an integer between 1 and 1000", env)
		}
		return count, nil
	}
	maxConnection, err := parsePositive("--max_connection", "MAX_CONNECTION", 4)
	if err != nil {
		return proxyCLIConfig{}, err
	}
	concurrency, err := parsePositive("--concurrency", "CONCURRENCY", 4)
	if err != nil {
		return proxyCLIConfig{}, err
	}
	if concurrency > maxConnection {
		concurrency = maxConnection
	}
	return proxyCLIConfig{
		maxConnection: maxConnection,
		concurrency: concurrency,
		proxyHubURL: get("--proxy_hub_url", "PROXY_HUB_URL"),
		proxyListTXT: get("--proxy_list_txt", "PROXY_LIST_TXT"),
	}, nil
}

func (cfg proxyCLIConfig) enabled() bool {
	return cfg.proxyHubURL != "" || cfg.proxyListTXT != ""
}

func loadCLIProxyPool(ctx context.Context, cfg proxyCLIConfig) (*connect.ProxyPool, error) {
	if !cfg.enabled() {
		return nil, nil
	}
	var proxies []*connect.ProxySettings
	if cfg.proxyListTXT != "" {
		fromFile, err := connect.LoadProxyFile(cfg.proxyListTXT)
		if err != nil {
			log.Printf("proxy file warning: %v", err)
		}
		proxies = append(proxies, fromFile...)
		log.Printf("proxy file: %d loaded", len(fromFile))
	}
	if cfg.proxyHubURL != "" {
		fromHub, err := (&connect.ProxyHubClient{BaseURL: cfg.proxyHubURL}).FetchOnce(ctx)
		if err != nil {
			log.Printf("ProxyHub warning: %v", err)
		}
		proxies = append(proxies, fromHub...)
		log.Printf("ProxyHub: %d loaded", len(fromHub))
	}
	pool, err := connect.NewProxyPool(proxies)
	if err != nil {
		return nil, fmt.Errorf("proxy mode requested but no valid proxies available: %w", err)
	}
	return pool, nil
}

func proxyOptions(opts docopt.Opts) (proxyCLIConfig, error) {
	dotenv, err := readProxyDotEnv(".env")
	if err != nil {
		return proxyCLIConfig{}, fmt.Errorf("read .env: %w", err)
	}
	return readProxyCLIConfig(opts, dotenv)
}

func proxyCheck(opts docopt.Opts) {
	cfg, err := proxyOptions(opts)
	if err != nil {
		log.Fatal(err)
	}
	if !cfg.enabled() {
		log.Fatal("set PROXY_HUB_URL or PROXY_LIST_TXT, or use the equivalent CLI flags")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := loadCLIProxyPool(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Proxy sources OK. Unique proxies: %d | MAX_CONNECTION=%d | CONCURRENCY=%d\n",
		pool.Len(), cfg.maxConnection, cfg.concurrency)
}

// launchProxyTransports adds H1-only platform transports. The default family
// transport group intentionally uses direct pinned strategies and cannot be
// used in proxy mode. Startup concurrency limits the number of initial
// connection attempts, not long-lived automatic reconnects.
func launchProxyTransports(
	ctx context.Context,
	cfg proxyCLIConfig,
	pool *connect.ProxyPool,
	routeManager *connect.RouteManager,
	connectURL string,
	auth *connect.ClientAuth,
) (func(), error) {
	type handle struct {
		strategy *connect.ClientStrategy
		transport *connect.PlatformTransport
	}
	handles := make([]handle, 0, cfg.maxConnection)
	sem := make(chan struct{}, cfg.concurrency)
	leaseCtx, releaseLeases := context.WithCancel(ctx)
	var leases sync.WaitGroup
	closeAll := func() {
		releaseLeases()
		for _, h := range handles {
			h.transport.Close()
		}
		leases.Wait()
		waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, h := range handles {
			_ = h.transport.CloseAndWait(waitCtx)
			h.strategy.Close()
		}
	}
	for i := 0; i < cfg.maxConnection; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			closeAll()
			return nil, ctx.Err()
		}
		strategy, err := connect.NewProxyClientStrategy(ctx, pool.Next())
		if err != nil {
			<-sem
			closeAll()
			return nil, fmt.Errorf("proxy connection %d: %w", i+1, err)
		}
		// A separate instance identifier avoids accidentally claiming
		// every physical connection is one platform instance.
		connAuth := *auth
		if i > 0 {
			connAuth.InstanceId = connect.NewId()
		}
		transportSettings := connect.DefaultPlatformTransportSettings()
		transportSettings.AltUrl = ""
		transport := connect.NewPlatformTransportWithTargetMode(
			ctx, strategy, routeManager,
			strings.TrimRight(connectURL, "/")+"/",
			&connAuth, connect.TransportModeH1, transportSettings,
		)
		handles = append(handles, handle{strategy: strategy, transport: transport})
		leases.Add(1)
		go func(transport *connect.PlatformTransport) {
			defer leases.Done()
			defer func() { <-sem }()
			timeout := time.NewTimer(15 * time.Second)
			defer timeout.Stop()
			for {
				notify := transport.ConnectedNotify()
				if transport.IsConnected() {
					return
				}
				select {
				case <-notify:
				case <-transport.Done():
					return
				case <-leaseCtx.Done():
					return
				case <-timeout.C:
					return
				}
			}
		}(transport)
	}
	fmt.Printf("Started %d H1 proxy transports (startup concurrency %d, pool %d).\n",
		len(handles), cfg.concurrency, pool.Len())
	return closeAll, nil
}
