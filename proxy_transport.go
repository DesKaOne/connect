package connect

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ProxyProtocol is the TCP proxy handshake protocol. HTTPS proxy URLs are not
// accepted: HTTP CONNECT here means a plaintext connection to the proxy.
type ProxyProtocol string

const (
	ProxyProtocolSOCKS5 ProxyProtocol = "socks5"
	ProxyProtocolSOCKS4 ProxyProtocol = "socks4"
	ProxyProtocolHTTP   ProxyProtocol = "http"
)

const proxyHandshakeTimeout = 15 * time.Second

// newProxyDialContext extends the existing SOCKS5 dialer with HTTP CONNECT and
// SOCKS4a while preserving SOCKS5 as the zero-value/default protocol.
func (self *ProxySettings) newProxyDialContext(forward proxy.Dialer) DialContextFunction {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if self == nil {
			return nil, errors.New("nil proxy settings")
		}
		if !strings.HasPrefix(network, "tcp") {
			return nil, fmt.Errorf("proxy only supports TCP, got %q", network)
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("invalid proxy destination: %w", err)
		}

		protocol := self.Protocol
		if protocol == "" {
			protocol = ProxyProtocolSOCKS5
		}

		switch protocol {
		case ProxyProtocolSOCKS5:
			proxyNetwork := self.Network
			if proxyNetwork == "" {
				proxyNetwork = "tcp"
			}
			dialer, err := proxy.SOCKS5(proxyNetwork, self.Address, self.Auth, forward)
			if err != nil {
				return nil, err
			}
			if withContext, ok := dialer.(proxy.ContextDialer); ok {
				return withContext.DialContext(ctx, network, addr)
			}
			return nil, errors.New("SOCKS5 dialer does not support context cancellation")
		case ProxyProtocolSOCKS4, ProxyProtocolHTTP:
			if self.Protocol == ProxyProtocolSOCKS4 && self.Auth != nil && self.Auth.Password != "" {
				return nil, errors.New("SOCKS4 does not support password authentication")
			}
			conn, err := proxyForwardDial(ctx, forward, self.Address)
			if err != nil {
				return nil, err
			}
			// Cancel a stuck handshake even without a context deadline. A
			// successfully handed-off connection outlives this callback.
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			deadline := time.Now().Add(proxyHandshakeTimeout)
			if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
				deadline = d
			}
			if err := conn.SetDeadline(deadline); err != nil {
				_ = conn.Close()
				return nil, err
			}
			var result net.Conn
			if protocol == ProxyProtocolHTTP {
				result, err = proxyHTTPConnect(conn, addr, self.Auth)
			} else {
				result, err = proxySOCKS4Connect(conn, addr, self.Auth)
			}
			if err == nil {
				err = ctx.Err()
			}
			if err == nil {
				err = result.SetDeadline(time.Time{})
			}
			if err != nil {
				_ = conn.Close()
				return nil, err
			}
			return result, nil
		default:
			return nil, fmt.Errorf("unsupported proxy protocol %q", protocol)
		}
	}
}

func proxyForwardDial(ctx context.Context, forward proxy.Dialer, address string) (net.Conn, error) {
	if forward == nil {
		return nil, errors.New("proxy forward dialer is nil")
	}
	// NetDialer(), supplied by ConnectSettings, implements ContextDialer.
	withContext, ok := forward.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("proxy forward dialer does not support context cancellation")
	}
	return withContext.DialContext(ctx, "tcp", address)
}

type proxyReadBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *proxyReadBufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func proxyHTTPConnect(conn net.Conn, address string, auth *proxy.Auth) (net.Conn, error) {
	if strings.ContainsAny(address, "\r\n") {
		return nil, errors.New("invalid HTTP CONNECT destination")
	}
	// A CONNECT request must not proxy arbitrary user-supplied headers.
	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", address, address)
	if auth != nil {
		value := base64.StdEncoding.EncodeToString([]byte(auth.User + ":" + auth.Password))
		fmt.Fprintf(&request, "Proxy-Authorization: Basic %s\r\n", value)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("read HTTP CONNECT response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP CONNECT refused: status %d", response.StatusCode)
	}
	// Preserve any payload bytes already buffered along with the headers.
	return &proxyReadBufferedConn{Conn: conn, reader: reader}, nil
}

func proxySOCKS4Connect(conn net.Conn, address string, auth *proxy.Auth) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid SOCKS4 destination port")
	}
	user := ""
	if auth != nil {
		user = auth.User
	}
	if strings.ContainsRune(user, 0) || strings.ContainsRune(host, 0) {
		return nil, errors.New("SOCKS4 credentials or host contain NUL")
	}
	// SOCKS4a encodes DNS names after a 0.0.0.1 sentinel so the proxy
	// resolves the destination, avoiding local DNS leakage.
	packet := []byte{4, 1, byte(port >> 8), byte(port)}
	ip, ipErr := netip.ParseAddr(host)
	switch {
	case ipErr == nil && ip.Is4():
		v4 := ip.As4()
		packet = append(packet, v4[:]...)
	case ipErr == nil:
		return nil, errors.New("SOCKS4 does not support IPv6 destinations")
	case host == "":
		return nil, errors.New("empty SOCKS4 host")
	default:
		packet = append(packet, 0, 0, 0, 1)
	}
	packet = append(packet, user...)
	packet = append(packet, 0)
	if ipErr != nil {
		packet = append(packet, host...)
		packet = append(packet, 0)
	}
	if _, err := conn.Write(packet); err != nil {
		return nil, err
	}
	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return nil, fmt.Errorf("read SOCKS4 reply: %w", err)
	}
	if reply[0] != 0 || reply[1] != 90 {
		return nil, fmt.Errorf("SOCKS4 connect refused: code %d", reply[1])
	}
	return conn, nil
}
