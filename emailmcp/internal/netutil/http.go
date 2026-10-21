package netutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// SafeHTTPClient returns an HTTP client that only dials public addresses
// (SSRF protection) and uses the given timeout for dial, TLS, and overall request.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy:                 nil, // never honor HTTP_PROXY for attachment fetches
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
				port = "443"
			}
			if err := ValidatePublicHostContext(ctx, host); err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve host %q: %w", host, err)
			}
			var last error
			for _, ip := range ips {
				if isBlockedIP(ip.IP) {
					last = fmt.Errorf("host %q resolves to a blocked address (%s)", host, ip.IP)
					continue
				}
				target := net.JoinHostPort(ip.IP.String(), port)
				c, err := dialer.DialContext(ctx, network, target)
				if err == nil {
					return c, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("host %q resolved to no addresses", host)
			}
			return nil, last
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}
