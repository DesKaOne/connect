package connect

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func TestParseProxySettings(t *testing.T) {
	cases := []struct {
		raw string
		want ProxyProtocol
		ok bool
	}{
		{"socks5://user:pass@127.0.0.1:1080", ProxyProtocolSOCKS5, true},
		{"socks4://127.0.0.1:1080", ProxyProtocolSOCKS4, true},
		{"http://user:pass@127.0.0.1:8080", ProxyProtocolHTTP, true},
		{"https://user:pass@127.0.0.1:8443", ProxyProtocolHTTPS, true},
		{"https://127.0.0.1:443", ProxyProtocolHTTPS, true},
		{"socks4://user:password@127.0.0.1:1080", "", false},
		{"http://127.0.0.1:0", "", false},
		{"http://127.0.0.1:99999", "", false},
		{"http://127.0.0.1:8080/path", "", false},
	}
	for _, test := range cases {
		t.Run(test.raw, func(t *testing.T) {
			cfg, err := ParseProxySettings(test.raw)
			if (err == nil) != test.ok {
				t.Fatalf("parse error = %v, want valid = %t", err, test.ok)
			}
			if test.ok && cfg.Protocol != test.want {
				t.Fatalf("protocol = %s, want %s", cfg.Protocol, test.want)
			}
		})
	}
}

func TestProxyPoolStickyRoundRobin(t *testing.T) {
	one, _ := ParseProxySettings("socks5://127.0.0.1:1001")
	two, _ := ParseProxySettings("http://127.0.0.1:1002")
	pool, err := NewProxyPool([]*ProxySettings{one, one, two})
	if err != nil || pool.Len() != 2 {
		t.Fatalf("pool = %v, err = %v", pool, err)
	}
	for _, want := range []string{one.Address, two.Address, one.Address} {
		if got := pool.Next().Address; got != want {
			t.Fatalf("address = %s, want %s", got, want)
		}
	}
}

func TestLoadProxyFileRetainsValidLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.txt")
	content := "# comment\n\nsocks5://localhost:1080\nnot-a-proxy\nhttp://localhost:8080\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	proxies, err := LoadProxyFile(path)
	if len(proxies) != 2 || err == nil {
		t.Fatalf("proxies = %d, err = %v", len(proxies), err)
	}
}

func TestProxyHubFetchOnce(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alive") != "true" {
			t.Errorf("missing alive filter")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/proxy/socks5":
			io.WriteString(w, `{"total_pages":1,"proxies":[{"address":"127.0.0.1:1080"}]}`)
		case "/proxy/http":
			io.WriteString(w, `{"total_pages":1,"proxies":[{"address":"127.0.0.1:8080"}]}`)
		case "/proxy/https":
			io.WriteString(w, `{"total_pages":1,"proxies":[{"address":"127.0.0.1:8443"}]}`)
		default:
			io.WriteString(w, `{"total_pages":1,"proxies":[]}`)
		}
	}))
	defer hub.Close()
	client := &ProxyHubClient{BaseURL: hub.URL}
	proxies, err := client.FetchOnce(context.Background())
	if err != nil || len(proxies) != 3 {
		t.Fatalf("proxies = %d, err = %v", len(proxies), err)
	}
	var foundHTTPS bool
	for _, cfg := range proxies {
		if cfg.Protocol == ProxyProtocolHTTPS && cfg.Address == "127.0.0.1:8443" {
			foundHTTPS = true
		}
	}
	if !foundHTTPS {
		t.Fatalf("HTTPS proxy was not discovered: %+v", proxies)
	}
}

func TestHTTPConnectHandshake(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		request, err := http.ReadRequest(reader)
		if err != nil {
			done <- err
			return
		}
		if request.Method != http.MethodConnect || request.Host != "example.com:443" {
			done <- errors.New("incorrect CONNECT authority")
			return
		}
		_, err = io.WriteString(server, "HTTP/1.1 200 Connection Established\r\n\r\nhello")
		done <- err
	}()
	defer client.Close()
	conn, err := proxyHTTPConnect(client, "example.com:443", nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("buffered tunnel data = %q, err = %v", buf, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSOCKS4aHandshake(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		header := make([]byte, 8)
		if _, err := io.ReadFull(server, header); err != nil {
			done <- err
			return
		}
		if header[0] != 4 || header[1] != 1 || header[4] != 0 || header[7] != 1 {
			done <- errors.New("expected SOCKS4a request")
			return
		}
		reader := bufio.NewReader(server)
		user, err := reader.ReadString(0)
		if err != nil || user != "\x00" {
			done <- errors.New("invalid userid")
			return
		}
		host, err := reader.ReadString(0)
		if err != nil || host != "example.com\x00" {
			done <- errors.New("invalid hostname")
			return
		}
		_, err = server.Write([]byte{0, 90, 0, 0, 0, 0, 0, 0})
		done <- err
	}()
	defer client.Close()
	_, err := proxySOCKS4Connect(client, "example.com:443", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProxyDialContextCancellation(t *testing.T) {
	cfg := &ProxySettings{Protocol: ProxyProtocolHTTP, Address: "127.0.0.1:8080"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dialer := cfg.NewDialContext(ctx, &net.Dialer{Timeout: time.Second})
	_, err := dialer(ctx, "tcp", "example.com:443")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestProxyRejectsUDP(t *testing.T) {
	cfg := &ProxySettings{Address: "127.0.0.1:1080"}
	dialer := cfg.NewDialContext(context.Background(), &net.Dialer{})
	_, err := dialer(context.Background(), "udp", "example.com:53")
	if err == nil || !strings.Contains(err.Error(), "TCP") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Keep compile-time contract explicit; external dialers must supply context.
var _ proxy.ContextDialer = (*net.Dialer)(nil)
