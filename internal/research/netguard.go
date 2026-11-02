// Package research implements web search (Wikipedia, DuckDuckGo) and page
// fetching for the Research agent. Only search queries and fetched URLs leave
// the device; all summarisation happens on the local model.
package research

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// IsPublicIP reports whether ip is a globally routable unicast address.
func IsPublicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}

// publicOnly is a net.Dialer Control hook: it runs after DNS resolution for
// every connection (including redirects), so URLs proposed by the model can
// never reach the assistant's own API, llama-server, or the user's LAN.
func publicOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if !IsPublicIP(net.ParseIP(host)) {
		return fmt.Errorf("blocked non-public address %s", host)
	}
	return nil
}

// NewPublicHTTPClient returns an HTTP client that can only reach public IPs.
func NewPublicHTTPClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}
	tr := &http.Transport{
		DialContext:           d.DialContext,
		Proxy:                 nil,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}
