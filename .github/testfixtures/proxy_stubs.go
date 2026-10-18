// CI-only stubs for verifying the standalone proxy code. This file is copied
// to an isolated temporary module by proxy-https.yml; it is not part of the
// production github.com/urnetwork/connect build.
package connect

import (
	"context"
	"net"

	"golang.org/x/net/proxy"
)

type ProxySettings struct {
	Protocol ProxyProtocol
	Network  string
	Address  string
	Auth     *proxy.Auth
}

type DialContextFunction func(context.Context, string, string) (net.Conn, error)

func (s *ProxySettings) NewDialContext(_ context.Context, forward proxy.Dialer) DialContextFunction {
	return s.newProxyDialContext(forward)
}

type ClientStrategySettings struct {
	ProxySettings *ProxySettings
	ExtenderConfigs any
	ExtenderDirectory any
	VlessConfigs any
	AltUrl string
	DisableManualExtenders bool
	DisableVless bool
}

type ClientStrategy struct{}

func DefaultClientStrategySettings() *ClientStrategySettings {
	return &ClientStrategySettings{}
}

func NewClientStrategy(_ context.Context, _ *ClientStrategySettings) *ClientStrategy {
	return &ClientStrategy{}
}
