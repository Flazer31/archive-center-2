// Package serve opens the backend's listeners on all addresses by default,
// or on the addresses the user chose, plain HTTP or HTTPS.
package serve

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

const DefaultPort = 28190
const DefaultListen = "all"

type Config struct {
	Listen  string // comma separated IPs, or "all" (default)
	Port    int
	TLSCert string // with TLSKey: serve HTTPS
	TLSKey  string
}

// Addrs returns the listen addresses ("host:port").
func (c Config) Addrs() ([]string, error) {
	listen := c.Listen
	if strings.TrimSpace(listen) == "" {
		listen = DefaultListen
	}
	var out []string
	for _, h := range strings.Split(listen, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if h == "all" {
			h = "0.0.0.0"
		}
		ip := net.ParseIP(h)
		if ip == nil {
			return nil, fmt.Errorf("listen address %q is not an IP address", h)
		}
		out = append(out, net.JoinHostPort(h, fmt.Sprint(c.Port)))
	}
	if len(out) == 0 {
		return nil, errors.New("no listen address")
	}
	return out, nil
}

// CheckAvailable reports a startup conflict before the managed launcher starts
// MariaDB. Start still owns the actual listeners (including later bind errors).
func (c Config) CheckAvailable() error {
	addrs, err := c.Addrs()
	if err != nil {
		return err
	}
	busy := func(err error) error {
		return fmt.Errorf("%d 포트를 열 수 없습니다. Archive Center 2가 이미 켜져 있거나 다른 프로그램이 이 포트를 쓰고 있습니다. 다른 시작 창을 닫고 다시 실행해 주세요: %w", c.Port, err)
	}
	// A backend on loopback only (Beta 0.0.1) does not always stop a wildcard
	// bind on Windows, and this PC would then still reach that backend.
	for _, addr := range addrs {
		if host, port, _ := net.SplitHostPort(addr); net.ParseIP(host).Equal(net.IPv4zero) {
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				return busy(err)
			}
			l.Close()
		}
	}
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for _, addr := range addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return busy(err)
		}
		listeners = append(listeners, l)
	}
	return nil
}

// ConnectionInfo is shared by the startup console and the connection tab.
// RemoteURLs contains only this machine's IPv4 addresses served by Config.
type ConnectionInfo struct {
	LocalURL   string   `json:"local_url,omitempty"`
	RemoteURLs []string `json:"remote_urls"`
	RemoteHint string   `json:"remote_hint"`
}

func (c Config) ConnectionInfo() (ConnectionInfo, error) {
	return c.connectionInfoFrom(interfaceAddresses)
}

func interfaceAddresses() ([]net.Addr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var addrs []net.Addr
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addresses...)
	}
	return addrs, nil
}

func (c Config) connectionInfoFrom(read func() ([]net.Addr, error)) (ConnectionInfo, error) {
	addrs, readErr := read()
	info, err := c.connectionInfo(addrs)
	if err != nil || readErr == nil {
		return info, err
	}
	info.RemoteHint = "이 기기의 네트워크 주소를 읽을 수 없어요(운영체제 네트워크 조회 제한)."
	if info.LocalURL != "" {
		info.RemoteHint += " 같은 기기에서는 " + info.LocalURL + "을 넣으세요."
	}
	bound, _ := c.Addrs()
	for _, addr := range bound {
		host, _, _ := net.SplitHostPort(addr)
		if !net.ParseIP(host).IsLoopback() {
			scheme := "http"
			if c.TLSCert != "" {
				scheme = "https"
			}
			info.RemoteHint += fmt.Sprintf(" 다른 기기에서는 %s://<이 기기의 IP>:%d(예: Tailscale 앱에 표시된 주소)를 넣으세요.", scheme, c.Port)
			return info, readErr
		}
	}
	info.RemoteHint += " 이 기기에서만 받도록 설정되어 있어 다른 기기에서는 연결할 수 없습니다."
	return info, readErr
}

func (c Config) connectionInfo(interfaces []net.Addr) (ConnectionInfo, error) {
	info := ConnectionInfo{RemoteURLs: []string{}}
	addrs, err := c.Addrs()
	if err != nil {
		return info, err
	}
	scheme := "http"
	if c.TLSCert != "" {
		scheme = "https"
	}
	url := func(host string) string { return scheme + "://" + net.JoinHostPort(host, fmt.Sprint(c.Port)) }
	var bound []net.IP
	loopbackOnly := true
	for _, addr := range addrs {
		host, _, _ := net.SplitHostPort(addr)
		ip := net.ParseIP(host)
		bound = append(bound, ip)
		if !ip.IsLoopback() {
			loopbackOnly = false
		}
		if ip.IsUnspecified() || ip.IsLoopback() {
			local := host
			if ip.IsUnspecified() {
				local = "127.0.0.1"
				if ip.To4() == nil {
					local = "::1"
				}
			}
			info.LocalURL = url(local)
		}
	}
	_, tailscale, _ := net.ParseCIDR("100.64.0.0/10")
	seen := map[string]bool{}
	for _, addr := range interfaces {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		for _, b := range bound {
			if !b.IsUnspecified() && !b.Equal(ip) {
				continue
			}
			value := url(ip.String())
			if tailscale.Contains(ip) {
				value += " (Tailscale)"
			}
			if !seen[value] {
				info.RemoteURLs = append(info.RemoteURLs, value)
				seen[value] = true
			}
			break
		}
	}
	sort.Strings(info.RemoteURLs)
	switch {
	case loopbackOnly:
		info.RemoteHint = "이 PC에서만 받도록 설정되어 있어요. 다른 기기에서는 연결할 수 없습니다."
	case len(info.RemoteURLs) == 0:
		info.RemoteHint = "다른 기기에서 쓸 IPv4 주소를 찾지 못했어요. 이 PC의 네트워크 연결과 수신 주소를 확인해 주세요."
	default:
		info.RemoteHint = "다른 기기에서 쓸 주소: " + strings.Join(info.RemoteURLs, ", ")
	}
	return info, nil
}

// Start listens on every address and serves handler; it returns once all
// listeners are open, and the servers to close.
func Start(c Config, handler http.Handler) ([]*http.Server, []net.Listener, <-chan error, error) {
	addrs, err := c.Addrs()
	if err != nil {
		return nil, nil, nil, err
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, nil, nil, errors.New("HTTPS needs both a certificate and a key file")
	}
	errs := make(chan error, len(addrs))
	var servers []*http.Server
	var listeners []net.Listener
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, l := range listeners {
				l.Close()
			}
			return nil, nil, nil, fmt.Errorf("listen %s: %w", a, err)
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		listeners = append(listeners, ln)
		go func() {
			if c.TLSCert != "" {
				errs <- srv.ServeTLS(ln, c.TLSCert, c.TLSKey)
			} else {
				errs <- srv.Serve(ln)
			}
		}()
	}
	return servers, listeners, errs, nil
}
