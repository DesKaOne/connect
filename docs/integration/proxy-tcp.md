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

## connectctl CLI (the actual binary)

The executable is `go run ./connectctl` — **not** `go run ./cmd/urnetwork`.
The existing `send` and `sink` subcommands work as before when no proxy
source is specified. `proxy-check` does not require a URnetwork JWT.

Create a local `.env` in the repository root (these files are gitignored):

```dotenv
MAX_CONNECTION=100
CONCURRENCY=10
PROXY_HUB_URL=http://127.0.0.1:8080
PROXY_LIST_TXT=proxy.txt
```

Both proxy sources are optional; at least one must supply valid proxies when
proxy mode is enabled. `MAX_CONNECTION` counts CLI **H1 platform transports**
(not guaranteed successful sessions). `CONCURRENCY` bounds *initial
connection starts* until connection registration or a 15-second allowance,
not future reconnects. Both counts are validated between 1 and 1000.

```powershell
go run ./connectctl --help
go run ./connectctl proxy-check
go run ./connectctl proxy-check --proxy_list_txt=proxy.txt --max_connection=10 --concurrency=3
go run ./connectctl proxy-test --max_connection=1 --concurrency=1
go run ./connectctl sink --jwt="<CLIENT_JWT>" --message_count=1
go run ./connectctl send --jwt="<CLIENT_JWT>" --destination_id="<DEST_CLIENT_ID>" "Hello URnetwork"
```

Flags `--proxy_hub_url`, `--proxy_list_txt`, `--max_connection`,
and `--concurrency` may also be passed to `send` or `sink`. Flags
override process environment, which overrides the local `.env` file.
A client JWT with `client_id` is required for `send`/`sink` (as before).

**Proxy mode does not provide UDP-based H3.** `send`/`sink` deliberately
use H1-only transports rather than the default IPv4/IPv6 pinned group:
the pinned group builds direct client strategies even when `ProxySettings`
is configured. This prevents that specific bypass in CLI transport creation;
not every SDK bootstrap or external networking path is covered by this feature.

`MAX_CONNECTION=100` can consume substantial resources and may trigger
server-side client limits. For a first live test use 1–3 and verify connections,
IP egress and registrations before increasing it. No client account, JWT, or
proxy is bundled in this repository.

## Debugging missing dashboard connections

`Started N H1 proxy transports` means construction only. In proxy mode,
`connectctl` now periodically logs `[proxy status] connected=N/M`, based
on the transport's registered route state. A value of zero means no registered
URnetwork transport has been confirmed. The backend dashboard may have
different refresh delays and its device rows are not one-per-transport.

Run `proxy-test` before testing a JWT:

```powershell
go run ./connectctl proxy-test --max_connection=1 --concurrency=1
```

This probes the selected first proxy's TCP tunnel to the `--connect_url`
backend and, for `wss://`, validates the TLS handshake. The command does
not connect a WebSocket or authenticate URnetwork, but it distinguishes a
dead proxy from a working TCP/TLS tunnel. The same pool loads the local
file **before** ProxyHub, so the first proxy is the first valid entry in
`proxy.txt`. To skip the file for a single PowerShell session and test
ProxyHub instead, set `$env:PROXY_LIST_TXT = ""`. Restore it with
`Remove-Item Env:PROXY_LIST_TXT` afterwards.

**Security**: proxy credentials are not printed by the diagnostic command,
but endpoint IP addresses and network errors are displayed. Do not paste
JWTs or credential-bearing URLs into shared terminal logs. `proxy-test`
does not guarantee that the URnetwork WebSocket or platform login will work.
