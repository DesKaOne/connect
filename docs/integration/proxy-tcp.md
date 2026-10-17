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

## connectctl public provider (distinct dashboard clients)

`connectctl sink` and `send` are message-test tools. Giving them
`--max_connection=2` creates transports **for a single Client JWT and
one client ID**; that is not equivalent to running two providers.

The new commands intentionally separate authentication from providing:

```powershell
# Use a new dashboard Auth Code; this securely prompts in PowerShell, then
# pipes it over stdin without putting the code on the process command line.
$secret = Read-Host "New URnetwork Auth Code" -AsSecureString
[System.Net.NetworkCredential]::new("", $secret).Password | go run ./connectctl auth
$secret = $null

# With PROXY_HUB_URL and/or PROXY_LIST_TXT in .env:
go run ./connectctl provide --max_connection=1 --concurrency=1
go run ./connectctl provide --max_connection=2 --concurrency=2
```

`auth` exchanges the dashboard code via `/auth/code-login` for a **network
JWT**, which it stores in `.urnetwork/jwt`. The file is local and gitignored.
Do not feed an existing Client JWT to this command.

`provide` uses the network JWT and calls `/network/auth-client` once per
new provider slot. Each slot receives a distinct Client JWT and a distinct
client ID. It configures `LocalUserNat`, `RemoteUserNatProvider`, Public
and Network provide modes, and `ClientAuth.ProvideIntent`. All provider
transports currently use H1/TCP. Client/instance IDs are saved under
`.urnetwork/connectctl_providers.json` and re-used on restart. Do not
run two provider processes with the same local ID state simultaneously.
After a connection fails, retries reuse its client ID rather than allocating
another. If the account's client limit is reached, startup stops for that
slot rather than retrying continuously.

`MAX_CONNECTION` counts **provider client identities**, not proxy endpoints
or WebSocket connections. `CONCURRENCY` caps simultaneous provider setup
requests (1–100), not reconnect attempts inside live transports. The
actual allowed client count depends on URnetwork account limits; start with
1–2 and verify the dashboard. All status logs distinguish registration from
`connected=true` (which indicates registered routes). Existing `sink`
behavior is unchanged.

One Webshare rotating gateway in `proxy.txt` can be reused by many provider
slots; whether each connection gets a distinct egress IP depends on Webshare's
rotation/session policy.

### Testing through mitmproxy

A local mitmproxy server normally **intercepts TLS**. URnetwork rejects
mitmproxy's replacement certificate unless the proxy's CA is trusted.
Never disable certificate validation to make this work.

For diagnostics, configure mitmproxy to **pass through** TLS to URnetwork.
For example (check your mitmproxy version's CLI syntax):

```powershell
mitmweb --mode regular@8080 --ignore-hosts '^(api|connect)\.bringyour\.com:443$'
```

Use `http://127.0.0.1:8080` in the proxy list for that listener.
For a SOCKS5 listener, use the corresponding `--mode socks5@1080`.
Passthrough means you may see connection metadata but not decrypted TLS
requests. Check that no other proxy process is bound to those ports.

The proxy feature provides **TCP tunneling**, not UDP/ICMP over the proxy.
That includes the platform connection and TCP NAT setup, but does not
guarantee every UDP/ICMP payload is proxied. Use packet capture to verify
actual egress before serving public traffic, especially through local
mitmproxy. Do not interpret a successful client registration alone as proof
the public provider is working.

The branch was not compiled or live-tested by this document update.
`go test ./connectctl ./...` requires repository sibling dependencies and
the Go toolchain declared in go.mod; validate locally before any merge.


## Provider event logging (sink-like output)

Managed `connectctl provide` now prints `connected=false/true` **only
on initial observation or a state change**, instead of repeating
`connected=true` every 30 seconds.

Each provider also attaches a bounded, nonblocking receive callback similar
to `connectctl sink`. Only these network-control frame types are printed:

- `TransferNetworkPeersReset`
- `TransferNetworkPeersUpdate`
- `TransferNetworkSessionsChanged`

Example, **only when the server actually sends such events**:

```text
[provider 1] connected=true
[provider 1 client=...] [ Network] [message_type:TransferNetworkPeersUpdate ...]
[provider 2 client=...] [ Network] [message_type:TransferNetworkSessionsChanged ...]
```

Unlike `sink`, a public provider may not receive network peer-list updates:
the URnetwork platform decides which control events it sends to each client.
**No event lines** after `connected=true` does not, by itself, indicate a
failure.

The logger does **not** print public-provider user traffic, IP packet
payloads or arbitrary received frames. The queue is limited to 256 events;
if the console cannot keep up, events are dropped and only the drop count is
reported. Network-control events can still contain device/client metadata,
so remove private identifiers before sharing terminal output.

An old provider's `Client does not exist.` response from
`/network/auth-client` is an identity/auth issue, not evidence of proxy
network failure: the locally saved `client_id` may no longer exist in the
network associated with the current Network JWT. Back up the identity store
and verify the account/network before resetting identities.


## Official provider uptime and traffic statistics

The URnetwork OpenAPI specification in `api/bringyour.yml` defines
`GET /stats/providers` with **network Bearer JWT** authentication. The
provider response includes a 24-hour window:

- `uptime_last_24h`: hours online (server measurement)
- `transfer_data_last_24h`: GiB of data transferred
- `payout_last_24h`: USDC paid/credited by the API
- `contracts_last_24h`, `clients_last_24h`: contract/client counts
- `connected`: the server's connected status at snapshot time

The new command fetches a single network-wide snapshot, without registering
or connecting any provider:

```powershell
go run ./connectctl stats
```

It uses the local network JWT in `.urnetwork/jwt`. If proxy mode is enabled,
the request uses the **first configured proxy**, never an intentional direct
fallback, and does not consume a provider's round-robin assignment slot.
Dashboard-generated API credentials may differ from this network JWT: the
documented endpoint uses Bearer JWT; we do not assume that an unspecified
dashboard API key is interchangeable.

During `provide`, summary output defaults to **once per 60 seconds**:

```powershell
go run ./connectctl provide --max_connection=2 --concurrency=2 --stats_interval=60
```

Set `--stats_interval=0` to disable periodic API/local summaries; 30 to
3600 seconds is allowed otherwise.

Example output format (illustrative values, not measured from your account):

```text
[provider 1] connected=true connect_ok=1 init_errors=0 disconnects=0
[local 1] connected=true connect_ok=1 init_errors=0 disconnects=0 online=1m0s runtime=1m3s
[local] connected=1/2 (since process start; not 24h server stats)
[stats] source=URnetwork API window=24h sampled_at=...
[stats provider 1] client=... server=online uptime_24h=7.25h transfer_24h=0.1250GiB payout_24h=0.001200USDC contracts=2 clients=1
```

Local `connect_ok` counts transitions into registered/connected state,
`init_errors` counts initial provider setup failures, and `disconnects`
counts transitions out of connected state. **They are not identical** to the
SDK's internal `normal/fragment success/error` dialer counters. Local
`online` time is accumulated since the **current process started**; it
does not equal server-side 24-hour uptime. All managed slots are covered by
**one API request per interval**, not one API call per client.

Stats polling failures produce a warning and leave the local counters
available. Server statistics may lag real-time events. Server stats are
filtered to the Client IDs persisted by this manager; newly registered
clients may not appear immediately in the provider stats API.

Official API reference:
- https://docs.ur.io/api/api-reference/stats/providers
- https://ur.io/openapi.yml
