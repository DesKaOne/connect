package connect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// ParseProxySettings accepts socks5://, socks4://, http:// and https:// proxy URLs.
// Percent-encode reserved characters inside usernames and passwords.
func ParseProxySettings(raw string) (*ProxySettings, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("proxy requires scheme://[user:pass@]host:port")
	}
	var proto ProxyProtocol
	switch strings.ToLower(u.Scheme) {
	case "socks5", "s5":
		proto = ProxyProtocolSOCKS5
	case "socks4", "s4":
		proto = ProxyProtocolSOCKS4
	case "http":
		proto = ProxyProtocolHTTP
	case "https":
		proto = ProxyProtocolHTTPS
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid proxy port")
	}
	cfg := &ProxySettings{Network: "tcp", Address: net.JoinHostPort(u.Hostname(), strconv.Itoa(port)), Protocol: proto}
	if u.User != nil {
		password, ok := u.User.Password()
		if !ok {
			return nil, errors.New("proxy username requires password")
		}
		cfg.Auth = &proxy.Auth{User: u.User.Username(), Password: password}
		if strings.TrimSpace(cfg.Auth.User) == "" {
			return nil, errors.New("empty proxy username")
		}
		if proto == ProxyProtocolSOCKS4 && password != "" {
			return nil, errors.New("SOCKS4 does not support passwords")
		}
	}
	return cfg, nil
}

// LoadProxyFile reads one URL per line, ignoring blank lines and comments.
// Bad entries are skipped and returned as a combined error; valid entries
// remain available. Callers can decide whether to reject partial input.
func LoadProxyFile(path string) ([]*ProxySettings, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var configs []*ProxySettings
	var problems []error
	for line := 1; scanner.Scan(); line++ {
		value := strings.TrimSpace(scanner.Text())
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		cfg, err := ParseProxySettings(value)
		if err != nil {
			problems = append(problems, fmt.Errorf("line %d: %w", line, err))
			continue
		}
		configs = append(configs, cfg)
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, err)
	}
	return configs, errors.Join(problems...)
}

// ProxyHubClient fetches an alive-proxy list from an operator-supplied API.
// FetchOnce makes one bounded pass; callers can schedule future refreshes.
type ProxyHubClient struct {
	BaseURL string
	Client *http.Client
	MaxPages int // 0 defaults to 20 pages per protocol
}

func (h *ProxyHubClient) FetchOnce(ctx context.Context) ([]*ProxySettings, error) {
	if h == nil {
		return nil, errors.New("nil ProxyHub client")
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(h.BaseURL), "/"))
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("ProxyHub URL must be an absolute HTTP(S) base URL")
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	maxPages := h.MaxPages
	if maxPages <= 0 {
		maxPages = 20
	}
	if maxPages > 100 {
		maxPages = 100
	}
	var configs []*ProxySettings
	var problems []error
	for _, protocol := range []ProxyProtocol{ProxyProtocolSOCKS5, ProxyProtocolHTTP, ProxyProtocolHTTPS, ProxyProtocolSOCKS4} {
		for page := 1; page <= maxPages; page++ {
			endpoint := *base
			endpoint.Path = strings.TrimRight(base.Path, "/") + "/proxy/" + string(protocol)
			query := endpoint.Query()
			query.Set("alive", "true")
			query.Set("page", strconv.Itoa(page))
			query.Set("limit", "100")
			endpoint.RawQuery = query.Encode()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
			if err != nil {
				return DeduplicateProxies(configs), err
			}
			resp, err := client.Do(req)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s page %d: %w", protocol, page, err))
				break
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				problems = append(problems, fmt.Errorf("%s page %d: HTTP %d", protocol, page, resp.StatusCode))
				break
			}
			var body struct {
				TotalPages int `json:"total_pages"`
				Proxies []struct {
					Address string `json:"address"`
				} `json:"proxies"`
			}
			err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
			resp.Body.Close()
			if err != nil {
				problems = append(problems, fmt.Errorf("%s page %d: %w", protocol, page, err))
				break
			}
			for _, item := range body.Proxies {
				address := strings.TrimSpace(item.Address)
				if !strings.Contains(address, "://") {
					address = string(protocol) + "://" + address
				}
				cfg, err := ParseProxySettings(address)
				if err == nil && cfg.Protocol == protocol {
					configs = append(configs, cfg)
				}
			}
			if page >= body.TotalPages || body.TotalPages < 1 {
				break
			}
		}
		if ctx.Err() != nil {
			return DeduplicateProxies(configs), ctx.Err()
		}
	}
	return DeduplicateProxies(configs), errors.Join(problems...)
}

// DeduplicateProxies preserves first-seen order without logging credentials.
func DeduplicateProxies(input []*ProxySettings) []*ProxySettings {
	seen := make(map[string]bool, len(input))
	out := make([]*ProxySettings, 0, len(input))
	for _, cfg := range input {
		if cfg == nil {
			continue
		}
		key := string(cfg.Protocol) + "\x00" + cfg.Address
		if cfg.Auth != nil {
			key += "\x00" + cfg.Auth.User + "\x00" + cfg.Auth.Password
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		copied := *cfg
		if cfg.Auth != nil {
			auth := *cfg.Auth
			copied.Auth = &auth
		}
		out = append(out, &copied)
	}
	return out
}

// ProxyPool assigns one stable proxy per logical client/session. Do not
// rotate an active TCP connection; call Next before constructing a new client.
type ProxyPool struct {
	configs []*ProxySettings
	next atomic.Uint64
}

func NewProxyPool(configs []*ProxySettings) (*ProxyPool, error) {
	snapshot := DeduplicateProxies(configs)
	if len(snapshot) == 0 {
		return nil, errors.New("empty proxy pool")
	}
	return &ProxyPool{configs: snapshot}, nil
}

func (p *ProxyPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.configs)
}

func (p *ProxyPool) Next() *ProxySettings {
	if p == nil || len(p.configs) == 0 {
		return nil
	}
	index := (p.next.Add(1) - 1) % uint64(len(p.configs))
	copied := *p.configs[index]
	if copied.Auth != nil {
		auth := *copied.Auth
		copied.Auth = &auth
	}
	return &copied
}

// First returns the first configured proxy without advancing round-robin
// state. Used for stats so monitoring does not shift provider assignments.
func (p *ProxyPool) First() *ProxySettings {
	if p == nil || len(p.configs) == 0 {
		return nil
	}
	cfg := *p.configs[0]
	if cfg.Auth != nil {
		auth := *cfg.Auth
		cfg.Auth = &auth
	}
	return &cfg
}

// NewProxyClientStrategy creates an API strategy with a stable proxy.
// It disables optional alternative paths which could bypass a TCP proxy.
// Note: separately constructed UDP/QUIC platform transports do not inherit
// this restriction and need their own proxy-capable packet transport.
func NewProxyClientStrategy(ctx context.Context, cfg *ProxySettings) (*ClientStrategy, error) {
	if cfg == nil {
		return nil, errors.New("nil proxy configuration")
	}
	settings := DefaultClientStrategySettings()
	settings.ProxySettings = cfg
	settings.ExtenderConfigs = nil
	settings.ExtenderDirectory = nil
	settings.VlessConfigs = nil
	settings.AltUrl = ""
	settings.DisableManualExtenders = true
	settings.DisableVless = true
	return NewClientStrategy(ctx, settings), nil
}
