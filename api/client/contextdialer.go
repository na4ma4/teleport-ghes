/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/gravitational/teleport/api/client/proxy"
	"github.com/gravitational/teleport/api/client/webclient"
	"github.com/gravitational/teleport/api/constants"
	tracessh "github.com/gravitational/teleport/api/observability/tracing/ssh"
	"github.com/gravitational/teleport/api/utils/sshutils"

	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
)

// ContextDialer represents network dialer interface that uses context
type ContextDialer interface {
	// DialContext is a function that dials the specified address
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// ContextDialerFunc is a function wrapper that implements the ContextDialer interface.
type ContextDialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// DialContext is a function that dials to the specified address
func (f ContextDialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

// newDirectDialer makes a new dialer to connect directly to an Auth server.
func newDirectDialer(keepAlivePeriod, dialTimeout time.Duration) ContextDialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: keepAlivePeriod,
	}
}

// NewDialer makes a new dialer that connects to an Auth server either directly or via an HTTP proxy, depending
// on the environment.
func NewDialer(keepAlivePeriod, dialTimeout time.Duration, tlsConfig *tls.Config) ContextDialer {
	return ContextDialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := newDirectOrHTTPConnUpgradeDialer(addr, keepAlivePeriod, dialTimeout, tlsConfig)
		if proxyURL := proxy.GetProxyURL(addr); proxyURL != nil {
			return DialProxyWithDialer(ctx, proxyURL, addr, dialer)
		}
		return dialer.DialContext(ctx, network, addr)
	})
}

// NewProxyDialer makes a dialer to connect to an Auth server through the SSH reverse tunnel on the proxy.
// The dialer will ping the web client to discover the tunnel proxy address on each dial.
func NewProxyDialer(ssh ssh.ClientConfig, keepAlivePeriod, dialTimeout time.Duration, discoveryAddr string, insecure bool) ContextDialer {
	dialer := newTunnelDialer(ssh, keepAlivePeriod, dialTimeout)
	return ContextDialerFunc(func(ctx context.Context, network, _ string) (conn net.Conn, err error) {
		tunnelAddr, err := webclient.GetTunnelAddr(
			&webclient.Config{Context: ctx, ProxyAddr: discoveryAddr, Insecure: insecure})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		conn, err = dialer.DialContext(ctx, network, tunnelAddr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return conn, nil
	})
}

// newTunnelDialer makes a dialer to connect to an Auth server through the SSH reverse tunnel on the proxy.
func newTunnelDialer(ssh ssh.ClientConfig, keepAlivePeriod, dialTimeout time.Duration) ContextDialer {
	dialer := newDirectDialer(keepAlivePeriod, dialTimeout)
	return ContextDialerFunc(func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
		conn, err = dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		sconn, err := sshConnect(ctx, conn, ssh, dialTimeout, addr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return sconn, nil
	})
}

// newTLSRoutingTunnelDialer makes a reverse tunnel TLS Routing dialer to connect to an Auth server
// through the SSH reverse tunnel on the proxy.
func newTLSRoutingTunnelDialer(ssh ssh.ClientConfig, keepAlivePeriod, dialTimeout time.Duration, discoveryAddr string, insecure bool) ContextDialer {
	return ContextDialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		tunnelAddr, err := webclient.GetTunnelAddr(
			&webclient.Config{Context: ctx, ProxyAddr: discoveryAddr, Insecure: insecure})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		host, _, err := webclient.ParseHostPort(tunnelAddr)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		tlsDialer := TLSRoutingDialer{
			KeepAlivePeriod: keepAlivePeriod,
			DialTimeout:     dialTimeout,
			Config: &tls.Config{
				NextProtos:         []string{constants.ALPNSNIProtocolReverseTunnel},
				InsecureSkipVerify: insecure,
				ServerName:         host,
			},
		}
		tlsConn, err := tlsDialer.DialContext(ctx, network, tunnelAddr)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		sconn, err := sshConnect(ctx, tlsConn, ssh, dialTimeout, tunnelAddr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return sconn, nil
	})
}

// sshConnect upgrades the underling connection to ssh and connects to the Auth service.
func sshConnect(ctx context.Context, conn net.Conn, ssh ssh.ClientConfig, dialTimeout time.Duration, addr string) (net.Conn, error) {
	ssh.Timeout = dialTimeout
	sconn, err := tracessh.NewClientConnWithDeadline(ctx, conn, addr, &ssh)
	if err != nil {
		return nil, trace.NewAggregate(err, conn.Close())
	}

	// Build a net.Conn over the tunnel. Make this an exclusive connection:
	// close the net.Conn as well as the channel upon close.
	conn, _, err = sshutils.ConnectProxyTransport(sconn.Conn, &sshutils.DialReq{
		Address: constants.RemoteAuthServer,
	}, true)
	if err != nil {
		return nil, trace.NewAggregate(err, sconn.Close())
	}
	return conn, nil
}

// TODO
func newDirectOrHTTPConnUpgradeDialer(proxyAddr string, keepAlivePeriod, dialTimeout time.Duration, tlsConfig *tls.Config) ContextDialer {
	if isHTTPConnUpgradeRequired(proxyAddr, tlsConfig) {
		return newHTTPConnUpgradeDialer(tlsConfig.InsecureSkipVerify)
	}

	return newDirectDialer(keepAlivePeriod, dialTimeout)
}

// TODO
type TLSRoutingDialer struct {
	KeepAlivePeriod time.Duration
	DialTimeout     time.Duration
	Config          *tls.Config
}

func (d *TLSRoutingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.Config == nil {
		return nil, trace.BadParameter("missing TLS config")
	}

	dialer := newDirectOrHTTPConnUpgradeDialer(addr, d.KeepAlivePeriod, d.DialTimeout, d.Config)
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	tlsConn := tls.Client(conn, d.Config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		defer conn.Close()
		return nil, trace.Wrap(err)
	}

	return tlsConn, nil
}
