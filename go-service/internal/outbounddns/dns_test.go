package outbounddns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/updateapi"
)

// No real DNS or provider is contacted, including in the negative control.
func isolatedDefaults(t *testing.T) {
	t.Helper()
	transport, client, resolver := http.DefaultTransport, http.DefaultClient, net.DefaultResolver
	base := transport.(*http.Transport).Clone()
	base.Proxy = nil
	http.DefaultTransport = base
	http.DefaultClient = &http.Client{Transport: base}
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("system DNS sentinel: external DNS forbidden in this test")
	}}
	t.Cleanup(func() {
		http.DefaultClient.CloseIdleConnections()
		base.CloseIdleConnections()
		http.DefaultTransport, http.DefaultClient, net.DefaultResolver = transport, client, resolver
	})
}

func TestUnsetOverridePreservesDefaults(t *testing.T) {
	isolatedDefaults(t)
	transport, client, resolver := http.DefaultTransport, http.DefaultClient, net.DefaultResolver
	for _, raw := range []string{"", "  ,  "} {
		if ConfigureOutboundDNSServers(raw) || http.DefaultTransport != transport || http.DefaultClient != client || net.DefaultResolver != resolver {
			t.Fatal("unset override changed an existing default")
		}
	}
}

func TestNormalizeMatchesAC1(t *testing.T) {
	got := normalizeDNSServers("1.1.1.1, 8.8.8.8:53, 2001:db8::1, [::1]:5353, dns.example, , bad:entry:port")
	want := []string{"1.1.1.1:53", "8.8.8.8:53", "[2001:db8::1]:53", "[::1]:5353", "dns.example:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

type fakeDNS struct {
	mu      sync.Mutex
	queries map[string][]uint16
	conn    net.PacketConn
}

func startDNS(t *testing.T) *fakeDNS {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDNS{queries: map[string][]uint16{}, conn: conn}
	done := make(chan struct{})
	t.Cleanup(func() { conn.Close(); <-done })
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			if len(q) < 17 || binary.BigEndian.Uint16(q[4:6]) != 1 {
				t.Error("unexpected DNS question")
				continue
			}
			pos := 12
			var labels []string
			for pos < len(q) && q[pos] != 0 {
				size := int(q[pos])
				pos++
				if size > 63 || pos+size > len(q) {
					break
				}
				labels = append(labels, string(q[pos:pos+size]))
				pos += size
			}
			if pos+5 > len(q) {
				t.Error("incomplete DNS question")
				continue
			}
			pos++
			kind := binary.BigEndian.Uint16(q[pos : pos+2])
			host := strings.Join(labels, ".")
			if !strings.HasSuffix(host, ".ac2.invalid") || (kind != 1 && kind != 28) {
				t.Errorf("unexpected DNS host/type: %s/%d", host, kind)
			}
			d.mu.Lock()
			d.queries[host] = append(d.queries[host], kind)
			d.mu.Unlock()
			reply := append([]byte(nil), q[:pos+4]...)
			binary.BigEndian.PutUint16(reply[2:4], 0x8180)
			binary.BigEndian.PutUint16(reply[6:8], 0)
			binary.BigEndian.PutUint16(reply[8:10], 0)
			binary.BigEndian.PutUint16(reply[10:12], 0)
			if kind == 1 {
				binary.BigEndian.PutUint16(reply[6:8], 1)
				reply = append(reply, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4, 127, 0, 0, 1)
			}
			if _, err := conn.WriteTo(reply, peer); err != nil {
				t.Errorf("DNS reply: %v", err)
			}
		}
	}()
	return d
}

func (d *fakeDNS) assertLookup(t *testing.T, host string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	kinds := d.queries[host]
	for _, kind := range kinds {
		if kind == 1 {
			t.Logf("loopback DNS received %s types=%v", host, kinds)
			return
		}
	}
	t.Fatalf("specified DNS received no A lookup for %s: %v", host, kinds)
}

func TestConfiguredResolverReachesProductionHTTP(t *testing.T) {
	isolatedDefaults(t)
	dns := startDNS(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/probe" || !strings.HasPrefix(r.Host, "probe.ac2.invalid:") {
			t.Errorf("unexpected HTTP request: %s %s", r.Host, r.URL.Path)
		}
		fmt.Fprint(w, "resolved")
	}))
	defer api.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(api.URL, "http://"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "http://probe.ac2.invalid:" + port + "/probe"
	// The old behavior must fail without sending anything to an external DNS.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
		t.Fatal("unconfigured system DNS unexpectedly resolved the synthetic host")
	}
	resolver := net.DefaultResolver
	if !ConfigureOutboundDNSServers(dns.conn.LocalAddr().String()) {
		t.Fatal("DNS override was not configured")
	}
	if net.DefaultResolver != resolver {
		t.Fatal("non-HTTP resolver was changed (including database dialing)")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "resolved" {
		t.Fatalf("unexpected HTTP body %q", body)
	}
	dns.assertLookup(t, "probe.ac2.invalid")
}

func TestProductionProviderAndUpdatePathsUseDNS(t *testing.T) {
	isolatedDefaults(t)
	dns := startDNS(t)
	ConfigureOutboundDNSServers(dns.conn.LocalAddr().String())
	cases := []struct {
		name, provider, path, response string
		embedding, contextual          bool
	}{
		{"neural-embed", "neuralwatt", "/v1/embeddings", `{"data":[{"embedding":[1,2]}]}`, true, false},
		{"voyage-embed", "voyageai", "/v1/embeddings", `{"data":[{"index":0,"embedding":[1,2]}]}`, true, false},
		{"voyage-context", "voyageai", "/v1/contextualizedembeddings", `{"data":[{"index":0,"data":[{"index":0,"embedding":[1,2]}]}]}`, true, true},
		{"ollama-embed", "ollama", "/api/embed", `{"embeddings":[[1,2]]}`, true, false},
		{"gemini-embed", "gemini", "/v1/models/fixture:embedContent", `{"embedding":{"values":[1,2]}}`, true, false},
		{"neural-critic", "neuralwatt", "/v1/chat/completions", `{"choices":[{"message":{"content":"fixture answer"}}]}`, false, false},
		{"ollama-critic", "ollama", "/api/chat", `{"message":{"content":"fixture answer"}}`, false, false},
		{"claude-critic", "claude", "/v1/messages", `{"content":[{"type":"text","text":"fixture answer"}]}`, false, false},
		{"gemini-critic", "gemini", "/v1/models/fixture:generateContent", `{"candidates":[{"content":{"parts":[{"text":"fixture answer"}]}}]}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.name + ".ac2.invalid"
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != tc.path || !strings.HasPrefix(r.Host, host+":") {
					t.Errorf("unexpected provider request: %s %s %s", r.Method, r.Host, r.URL.Path)
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) == 0 {
					t.Error("missing production provider body")
				}
				fmt.Fprint(w, tc.response)
			}))
			defer api.Close()
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(api.URL, "http://"))
			base := "http://" + host + ":" + port + "/v1"
			if tc.provider == "ollama" || tc.provider == "claude" {
				base = strings.TrimSuffix(base, "/v1")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tc.embedding {
				model := "fixture"
				if tc.contextual {
					model = "voyage-context-fixture"
				}
				e, err := provider.NewEmbedder(provider.EmbedConfig{Provider: tc.provider, Model: model, URL: base, Key: "synthetic"})
				if err != nil {
					t.Fatal(err)
				}
				if tc.contextual {
					out, err := e.EmbedGroups(ctx, [][]string{{"fixture input"}}, "query")
					if err != nil || !reflect.DeepEqual(out.Vectors, [][][]float32{{{1, 2}}}) {
						t.Fatalf("contextual response: %+v %v", out, err)
					}
				} else {
					out, err := e.Embed(ctx, []string{"fixture input"}, "query")
					if err != nil || !reflect.DeepEqual(out.Vectors, [][]float32{{1, 2}}) {
						t.Fatalf("embedding response: %+v %v", out, err)
					}
				}
			} else {
				p, err := provider.New(provider.Config{Provider: tc.provider, Model: "fixture", URL: base, Key: "synthetic"})
				if err != nil {
					t.Fatal(err)
				}
				out, err := p.Chat(ctx, "fixture input")
				if err != nil || out.Content != "fixture answer" {
					t.Fatalf("critic response: %+v %v", out, err)
				}
			}
			if calls != 1 {
				t.Fatalf("API calls=%d", calls)
			}
			dns.assertLookup(t, host)
			t.Logf("production HTTP reached %s %s", host, tc.path)
		})
	}
	t.Run("update-check", func(t *testing.T) {
		const host = "update.ac2.invalid"
		calls := 0
		api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != http.MethodGet || r.URL.Path != "/repos/fixture/project/releases/latest" || !strings.HasPrefix(r.Host, host+":") {
				t.Errorf("unexpected update request: %s %s %s", r.Method, r.Host, r.URL.Path)
			}
			fmt.Fprint(w, `{"tag_name":"v0.0.1","name":"fixture","assets":[]}`)
		}))
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		api.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
		api.StartTLS()
		defer api.Close()
		roots := x509.NewCertPool()
		roots.AddCert(api.Certificate())
		transport := http.DefaultTransport.(*http.Transport)
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(api.URL, "https://"))
		s := updateapi.New(updateapi.Config{Repository: "fixture/project", APIBase: "https://" + host + ":" + port, CurrentVersion: "0.0.1"})
		rec := httptest.NewRecorder()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/update/check", nil).WithContext(ctx))
		if calls != 1 || rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"latest_version":"0.0.1"`) {
			t.Fatalf("update status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
		}
		dns.assertLookup(t, host)
	})
}
