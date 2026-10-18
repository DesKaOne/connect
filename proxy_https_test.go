package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func TestHTTPSProxyCONNECTUsesTLSAndBasicAuth(t *testing.T) {
	seen := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("username:password"))
		if r.Method != http.MethodConnect || r.Host != "example.com:443" || r.Header.Get("Proxy-Authorization") != want {
			w.WriteHeader(http.StatusBadRequest)
			seen <- "bad CONNECT request"
			return
		}
		seen <- "ok"
		c, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\nhello")
		_ = buffered.Flush()
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tlsConn, err := proxyTLSConnect(context.Background(), raw, u.Host, &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()

	conn, err := proxyHTTPConnect(tlsConn, "example.com:443", &proxy.Auth{
		User: "username", Password: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-seen; result != "ok" {
		t.Fatalf("unexpected proxy request: %s", result)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "hello" {
		t.Fatalf("tunneled data = %q, err = %v", string(got), err)
	}
}

func TestHTTPSProxyDialRejectsUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg, err := ParseProxySettings(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	dial := cfg.NewDialContext(context.Background(), &net.Dialer{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dial(ctx, "tcp", "example.com:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("untrusted HTTPS proxy unexpectedly accepted")
	}
	if err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Fatalf("expected TLS certificate validation failure, got %v", err)
	}
}

func TestHTTPSProxyCONNECTRejects407(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	secured, err := proxyTLSConnect(context.Background(), raw, u.Host, &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	defer secured.Close()
	conn, err := proxyHTTPConnect(secured, "example.com:443", nil)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("407 CONNECT unexpectedly succeeded")
	}
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("expected proxy authentication required (407), got %v", err)
	}
}
