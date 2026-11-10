package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConnectionInfoInterfaceFailureStillExplainsConnection(t *testing.T) {
	failure := errors.New("route ip+net: netlinkrib: permission denied")
	for _, cfg := range []Config{
		{Listen: "all", Port: DefaultPort},
		{Listen: "127.0.0.1", Port: 31234},
		{Listen: "::", Port: 31235, TLSCert: "fixture"},
		{Listen: "192.0.2.7", Port: 31236},
	} {
		info, err := cfg.connectionInfoFrom(func() ([]net.Addr, error) { return nil, failure })
		if !errors.Is(err, failure) || info.RemoteURLs == nil || len(info.RemoteURLs) != 0 || !strings.Contains(info.RemoteHint, "네트워크 주소를 읽을 수 없어요") {
			t.Fatalf("missing connection guidance: %+v %v", info, err)
		}
		if info.LocalURL != "" && !strings.Contains(info.RemoteHint, info.LocalURL) {
			t.Fatalf("local endpoint missing: %+v", info)
		}
		if cfg.Listen == "127.0.0.1" {
			if strings.Contains(info.RemoteHint, "Tailscale") || !strings.Contains(info.RemoteHint, "다른 기기에서는 연결할 수 없습니다") {
				t.Fatal(info.RemoteHint)
			}
		} else if !strings.Contains(info.RemoteHint, fmt.Sprint(cfg.Port)) || !strings.Contains(info.RemoteHint, "Tailscale") {
			t.Fatal(info.RemoteHint)
		}
		t.Log(info.RemoteHint)
	}
}

func TestDefaultIsAllAddresses(t *testing.T) {
	addrs, err := Config{Port: 28190}.Addrs()
	if err != nil || len(addrs) != 1 || addrs[0] != "0.0.0.0:28190" {
		t.Fatalf("%v %v", addrs, err)
	}
}

// The default (all addresses) is checked through Addrs above; opening it here
// would make Windows ask for firewall access on every test build.
func TestExplicitLoopbackListener(t *testing.T) {
	servers, listeners, _, err := Start(Config{Listen: "127.0.0.1"}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	addr := listeners[0].Addr().(*net.TCPAddr).IP
	servers[0].Close()
	if !addr.IsLoopback() {
		t.Fatalf("explicit loopback widened to %s", addr)
	}
}

// A backend on loopback only (Beta 0.0.1) holds the port: the default (all
// addresses) must report it before binding the wildcard address.
func TestCheckAvailableSeesLoopbackOnlyBackend(t *testing.T) {
	old, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	port := old.Addr().(*net.TCPAddr).Port
	err = Config{Port: port}.CheckAvailable()
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(port)+" 포트") {
		t.Fatalf("a loopback-only backend on the port was not reported: %v", err)
	}
}

func TestConnectionInfoUsesActualInterfaces(t *testing.T) {
	cfg := Config{Port: DefaultPort}
	got, err := cfg.ConnectionInfo()
	if err != nil {
		t.Fatal(err)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
				want[ip.String()] = true
			}
		}
	}
	for _, value := range got.RemoteURLs {
		endpoint := strings.Split(value, " ")[0]
		host, _, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "http://"))
		if err != nil || !want[host] {
			t.Fatalf("address does not belong to a non-loopback IPv4 interface: %q", value)
		}
		delete(want, host)
	}
	if len(want) != 0 {
		t.Fatalf("actual interface addresses are missing: %v", want)
	}
}

func TestConnectionInfoLabelsAndListenScope(t *testing.T) {
	var interfaces []net.Addr
	for _, value := range []string{"127.0.0.1/8", "::1/128", "2001:db8::1/64", "192.0.2.7/24", "100.64.0.1/10", "100.127.255.254/10", "100.128.0.1/24"} {
		ip, block, err := net.ParseCIDR(value)
		if err != nil {
			t.Fatal(err)
		}
		block.IP = ip
		interfaces = append(interfaces, block)
	}
	cfg := Config{Port: DefaultPort}
	got, err := cfg.connectionInfo(interfaces)
	want := []string{"http://192.0.2.7:28190", "http://100.64.0.1:28190 (Tailscale)", "http://100.127.255.254:28190 (Tailscale)", "http://100.128.0.1:28190"}
	sort.Strings(want)
	if err != nil || !reflect.DeepEqual(got.RemoteURLs, want) || got.LocalURL != "http://127.0.0.1:28190" || got.RemoteHint != "다른 기기에서 쓸 주소: "+strings.Join(want, ", ") {
		t.Fatalf("incorrect connection details: %+v, %v", got, err)
	}
	cfg.Listen = "127.0.0.1"
	local, err := cfg.connectionInfo(interfaces)
	if err != nil || len(local.RemoteURLs) != 0 || !strings.Contains(local.RemoteHint, "이 PC에서만") {
		t.Fatalf("loopback configuration advertises remote access: %+v, %v", local, err)
	}
	cfg.Listen, cfg.TLSCert = "192.0.2.7", "test-certificate"
	one, err := cfg.connectionInfo(interfaces)
	if err != nil || !reflect.DeepEqual(one.RemoteURLs, []string{"https://192.0.2.7:28190"}) {
		t.Fatalf("explicit binding or HTTPS was ignored: %+v, %v", one, err)
	}
}

// A Tailscale IP, a LAN address or all addresses (DDNS) can be added, and
// several can be open at once; no token (as in AC1).
func TestSeveralAddresses(t *testing.T) {
	addrs, err := Config{Listen: "127.0.0.1, 100.64.0.10", Port: 28190}.Addrs()
	if err != nil || strings.Join(addrs, " ") != "127.0.0.1:28190 100.64.0.10:28190" {
		t.Fatalf("%v %v", addrs, err)
	}
	if addrs, err := (Config{Listen: "all", Port: 1}).Addrs(); err != nil || addrs[0] != "0.0.0.0:1" {
		t.Fatalf("all: %v %v", addrs, err)
	}
	if _, err := (Config{Listen: "host.example", Port: 1}).Addrs(); err == nil {
		t.Fatal("a host name is not a listen address")
	}
}

func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestServesHTTPOnEveryAddress(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	servers, _, _, err := Start(Config{Listen: "127.0.0.1", Port: port}, h)
	if err != nil {
		t.Fatal(err)
	}
	defer servers[0].Close()
	if got := get(t, http.DefaultClient, "http://127.0.0.1:"+strconv.Itoa(port)+"/"); got != "ok" {
		t.Fatal(got)
	}
}

// With a certificate and key the backend serves HTTPS (Tailscale cert or a
// DDNS domain's certificate).
func TestServesHTTPS(t *testing.T) {
	dir := t.TempDir()
	cert, key := selfSigned(t, dir)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") })
	servers, _, _, err := Start(Config{Listen: "127.0.0.1", Port: port, TLSCert: cert, TLSKey: key}, h)
	if err != nil {
		t.Fatal(err)
	}
	defer servers[0].Close()
	pool := x509.NewCertPool()
	certPEM, _ := os.ReadFile(cert)
	pool.AppendCertsFromPEM(certPEM)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if got := get(t, c, "https://127.0.0.1:"+strconv.Itoa(port)+"/"); got != "secure" {
		t.Fatal(got)
	}
	if _, _, _, err := Start(Config{Port: port + 1, TLSCert: cert}, h); err == nil {
		t.Fatal("certificate without key accepted")
	}
}

func selfSigned(t *testing.T, dir string) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ac2-test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	cert, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cert, key
}
