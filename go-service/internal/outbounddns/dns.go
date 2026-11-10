// Package outbounddns applies the optional process-wide outbound HTTP DNS
// override used by the Termux launcher. An unset override leaves Go unchanged.
package outbounddns

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// ConfigureOutboundDNSServers carries AC1's outbound resolver into AC2.
// Call before managed downloads, provider requests or update clients are made.
func ConfigureOutboundDNSServers(raw string) bool {
	servers := normalizeDNSServers(raw)
	if len(servers) == 0 {
		return false
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialer := net.Dialer{}
			var lastErr error
			for _, server := range servers {
				conn, err := dialer.DialContext(ctx, "udp", server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return dialer.DialContext(ctx, network, address)
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Resolver: resolver}).DialContext
	http.DefaultTransport = transport
	http.DefaultClient = &http.Client{Transport: transport}
	return true
}

func normalizeDNSServers(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if server := normalizeDNSServer(strings.TrimSpace(part)); server != "" {
			out = append(out, server)
		}
	}
	return out
}

func normalizeDNSServer(raw string) string {
	if raw == "" {
		return ""
	}
	if host, port, err := net.SplitHostPort(raw); err == nil && host != "" && port != "" {
		return net.JoinHostPort(host, port)
	}
	if ip := net.ParseIP(raw); ip != nil {
		return net.JoinHostPort(raw, "53")
	}
	if strings.Contains(raw, ":") {
		return ""
	}
	return net.JoinHostPort(raw, "53")
}
