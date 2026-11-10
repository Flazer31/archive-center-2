package main

import (
	"flag"
	"net/http"
	"os"
	"testing"
)

// The production entrypoint must configure HTTP before managed downloads or
// listener/DB work. An invalid existing listen option stops before any of those
// effects; the resolver's actual packets are tested in internal/outbounddns.
func TestBackendAppliesDNSBeforeStartup(t *testing.T) {
	args, flags := os.Args, flag.CommandLine
	transport, client := http.DefaultTransport, http.DefaultClient
	t.Cleanup(func() {
		http.DefaultClient.CloseIdleConnections()
		os.Args, flag.CommandLine = args, flags
		http.DefaultTransport, http.DefaultClient = transport, client
	})
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("AC2_DNS_SERVERS", "127.0.0.1:15353")
	os.Args = []string{"ac2", "-listen", "invalid-listen-fixture"}
	flag.CommandLine = flag.NewFlagSet("ac2-dns-test", flag.ContinueOnError)
	if code := runBackend(); code != 1 {
		t.Fatalf("existing invalid-listen preflight returned %d", code)
	}
	if http.DefaultTransport == transport || http.DefaultClient == client {
		t.Fatal("backend did not apply AC2_DNS_SERVERS before startup")
	}
}
