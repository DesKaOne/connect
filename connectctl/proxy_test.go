package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/docopt/docopt-go"
)

func TestProxyCheckUsageParses(t *testing.T) {
	for _, args := range [][]string{
		{"proxy-check"},
		{"proxy-check", "--proxy_list_txt=proxy.txt", "--max_connection=100", "--concurrency=10"},
		{"proxy-test", "--max_connection=1", "--concurrency=1"},
		{"proxy-test", "--connect_url=wss://connect.bringyour.com", "--proxy_list_txt=proxy.txt"},
		{"sink", "--jwt=example", "--proxy_hub_url=http://127.0.0.1:8080"},
		{"send", "--jwt=example", "--destination_id=example", "hello", "--proxy_list_txt=proxy.txt"},
	} {
		_, err := docopt.ParseArgs(connectCtlUsage(), args, ConnectCtlVersion)
		if err != nil {
			t.Errorf("parsing %v: %v", args, err)
		}
	}
}

func TestProxyConfigFlagOverridesEnvironment(t *testing.T) {
	t.Setenv("MAX_CONNECTION", "100")
	t.Setenv("CONCURRENCY", "10")
	t.Setenv("PROXY_LIST_TXT", "env-proxies.txt")
	args, err := docopt.ParseArgs(connectCtlUsage(),
		[]string{"proxy-check", "--max_connection=7", "--proxy_list_txt=flag-proxies.txt"},
		ConnectCtlVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readProxyCLIConfig(args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.maxConnection != 7 || cfg.concurrency != 7 || cfg.proxyListTXT != "flag-proxies.txt" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestProxyConfigDotenv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(
		"# comment\nMAX_CONNECTION=100\nCONCURRENCY=10\nPROXY_HUB_URL=http://127.0.0.1:8080\nPROXY_LIST_TXT='proxy.txt'\n",
	), 0600); err != nil {
		t.Fatal(err)
	}
	values, err := readProxyDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["MAX_CONNECTION"] != "100" || values["CONCURRENCY"] != "10" ||
		values["PROXY_HUB_URL"] != "http://127.0.0.1:8080" || values["PROXY_LIST_TXT"] != "proxy.txt" {
		t.Fatalf("dotenv = %#v", values)
	}
}

func TestProxyConfigRejectsInvalidLimits(t *testing.T) {
	t.Setenv("MAX_CONNECTION", "-1")
	args, err := docopt.ParseArgs(connectCtlUsage(), []string{"proxy-check"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readProxyCLIConfig(args, nil)
	if err == nil {
		t.Fatal("expected invalid MAX_CONNECTION error")
	}
}

func TestProxyTarget(t *testing.T) {
	cases := []struct {
		url string
		addr string
		host string
		tls bool
		valid bool
	}{
		{"wss://connect.bringyour.com", "connect.bringyour.com:443", "connect.bringyour.com", true, true},
		{"ws://localhost:9000/path", "localhost:9000", "localhost", false, true},
		{"wss://[::1]:8443/path", "[::1]:8443", "::1", true, true},
		{"http://connect.bringyour.com", "", "", false, false},
		{"", "", "", false, false},
	}
	for _, tc := range cases {
		addr, host, useTLS, err := proxyTarget(tc.url)
		if (err == nil) != tc.valid {
			t.Errorf("proxyTarget(%q) err=%v, want valid=%t", tc.url, err, tc.valid)
			continue
		}
		if tc.valid && (addr != tc.addr || host != tc.host || useTLS != tc.tls) {
			t.Errorf("proxyTarget(%q) got (%q,%q,%t)", tc.url, addr, host, useTLS)
		}
	}
}
