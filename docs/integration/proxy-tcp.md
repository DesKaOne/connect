# Optional outbound TCP proxies

This feature is isolated on `feature/proxy-hub-multi-proxy`. It does not change the default direct connection path.

## Supported

- SOCKS5 (existing behavior), with optional username/password.
- HTTP CONNECT (HTTP proxy endpoint, TCP only), with optional Basic authentication.
- SOCKS4 / SOCKS4a (remote DNS for domain names; optional username).
- Local text lists, ProxyHub pagination and round-robin assignment of a **stable proxy per client session**.

**Not supported by this feature:** HTTPS-to-proxy TLS endpoints, UDP ASSOCIATE, SOCKS5 UDP, QUIC/H3/WebRTC packet routing, health scoring or transparent in-session failover. A TCP proxy setting does **not** guarantee that a VPN's entire traffic uses that proxy.

## Local proxy file

```text
# one proxy per line
socks5://127.0.0.1:1080
socks5://username:password@127.0.0.1:1081
socks4://127.0.0.1:1082
http://username:password@127.0.0.1:3128
```

Credentials containing reserved URL characters must be percent-encoded. Do not commit live secrets.

## Integration example

```go
ctx := context.Background()
fromFile, fileErr := connect.LoadProxyFile("proxy.txt")
if fileErr != nil {
    log.Printf("some proxy lines failed: %v", fileErr)
}
hub := &connect.ProxyHubClient{BaseURL: os.Getenv("PROXY_HUB_URL")}
fromHub, hubErr := hub.FetchOnce(ctx)
if hubErr != nil {
    log.Printf("ProxyHub failed: %v", hubErr)
}
pool, err := connect.NewProxyPool(append(fromFile, fromHub...))
if err != nil {
    log.Fatal(err)
}
strategy, err := connect.NewProxyClientStrategy(ctx, pool.Next())
if err != nil {
    log.Fatal(err)
}
defer strategy.Close()
// Pass strategy to the owner of the API/WS connection and keep it
// for the lifetime of that logical client session.
```

`ProxyHubClient.FetchOnce` attempts `GET /proxy/{socks5,http,socks4}?alive=true&page=N&limit=100` on the supplied endpoint. Its bounded request client timeout defaults to 20 seconds, and each protocol is limited to 20 pages by default. It returns valid partial results **with** an error if a protocol failed. The operator must validate/trust the configured ProxyHub URL; the service can disclose proxy endpoints and credentials.

If you only need a single proxy:

```go
cfg, err := connect.ParseProxySettings("socks5://127.0.0.1:1080")
if err != nil {
    log.Fatal(err)
}
settings := connect.DefaultClientStrategySettings()
settings.ProxySettings = cfg
strategy := connect.NewClientStrategy(ctx, settings)
defer strategy.Close()
```

When using a raw `ProxySettings`, optional alternate egress strategies must be disabled separately if TCP proxy routing is mandatory. `NewProxyClientStrategy` disables configured extender/VLESS/Alt paths for the API strategy, but does not override separately constructed packet transports, and cannot promise fail-closed treatment across every internal bootstrap request.

## Verify

```bash
go test ./... 
go test -race ./... 
```

The upstream module has local replacements for `../glog`, `../gvisor`, and `./sctp`; ensure sibling checkouts match the repository layout. Tests were authored in this branch, but execution has not been verified by this change.
