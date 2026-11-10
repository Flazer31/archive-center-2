package observe

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type claimReply struct {
	Code    string `json:"code"`
	Token   string `json:"token"`
	Checked bool   `json:"checked"`
	Closed  bool   `json:"closed"`
	At      string `json:"claimed_at"`
	Address string `json:"address"`
}

func claimRequest(t *testing.T, h http.Handler, address string) (int, claimReply) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/settings/token/claim", nil)
	r.RemoteAddr = address
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out claimReply
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return w.Code, out
}

func claimServer(tok *Token) *Server {
	s := NewServer(&memSink{})
	s.Token = tok
	return s
}

func TestFirstTokenClaimOnceAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chosen-data", "token.txt")
	tok, made, err := LoadToken(path)
	if err != nil || !made {
		t.Fatalf("fresh token: made=%v err=%v", made, err)
	}
	s := claimServer(tok)
	at := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return at }
	address := httptest.NewRequest(http.MethodPost, "/", nil).RemoteAddr
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	h := s.Public()
	status, first := claimRequest(t, h, address)
	if status != http.StatusOK || first.Token != tok.Value() || !first.Checked || !first.Closed || first.At != at.Format(time.RFC3339) || first.Address != address {
		t.Fatalf("first claim: status=%d metadata=%+v token_matches=%v", status, withoutClaimToken(first), first.Token == tok.Value())
	}
	if strings.Contains(logs.String(), tok.Value()) || !strings.Contains(logs.String(), "첫 토큰이 "+address+"에 할당됨") || !strings.Contains(logs.String(), first.At) || strings.Count(logs.String(), "할당됨") != 1 {
		t.Fatal("assignment log must have one address/time line and no token")
	}
	raw, err := os.ReadFile(path + ".claim.json")
	var saved claimReply
	if err != nil || json.Unmarshal(raw, &saved) != nil || !saved.Closed || saved.At != first.At || saved.Address != first.Address || strings.Contains(string(raw), tok.Value()) {
		t.Fatalf("persisted assignment differs or contains token: err=%v", err)
	}
	assertClosed := func(h http.Handler) {
		t.Helper()
		code, next := claimRequest(t, h, "another-device")
		if code != http.StatusConflict || next.Code != "token_claim_closed" || next.Token != "" || next.At != first.At || next.Address != first.Address || !next.Closed {
			t.Fatalf("closed claim: status=%d reply=%+v", code, withoutClaimToken(next))
		}
	}
	assertClosed(h)
	if rec := send(h, http.MethodGet, "/version", first.Token, ""); rec.Code != http.StatusOK {
		t.Fatalf("assigned token cannot connect: %d", rec.Code)
	}
	again, made, err := LoadToken(path)
	if err != nil || made {
		t.Fatalf("restart: made=%v err=%v", made, err)
	}
	assertClosed(claimServer(again).Public())
	if _, _, err := again.Rotate(); err != nil {
		t.Fatal(err)
	}
	assertClosed(claimServer(again).Public())
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reopened, made, err := LoadToken(path)
	if err != nil || !made || reopened.Value() == tok.Value() {
		t.Fatalf("delete and restart: made=%v err=%v", made, err)
	}
	status, fresh := claimRequest(t, claimServer(reopened).Public(), "new-device")
	if status != http.StatusOK || fresh.Token != reopened.Value() || fresh.Address != "new-device" {
		t.Fatal("deleting token.txt did not reopen first assignment with the new token")
	}
}

func withoutClaimToken(v claimReply) claimReply { v.Token = ""; return v }

func TestFirstTokenClaimConcurrent(t *testing.T) {
	tok, _, err := LoadToken(filepath.Join(t.TempDir(), "token.txt"))
	if err != nil {
		t.Fatal(err)
	}
	h := claimServer(tok).Public()
	start := make(chan struct{})
	type result struct {
		code  int
		reply claimReply
	}
	done := make(chan result, 2)
	for _, address := range []string{"device-a", "device-b"} {
		go func() {
			<-start
			code, reply := claimRequest(t, h, address)
			done <- result{code, reply}
		}()
	}
	close(start)
	a, b := <-done, <-done
	if a.code != http.StatusOK {
		a, b = b, a
	}
	if a.code != http.StatusOK || a.reply.Token != tok.Value() || b.code != http.StatusConflict || b.reply.Token != "" || a.reply.At != b.reply.At || a.reply.Address != b.reply.Address {
		t.Fatalf("concurrent assignment: codes %d, %d; matching metadata=%v", a.code, b.code, a.reply.Address == b.reply.Address)
	}
}

func TestFirstTokenClaimExistingFileStartsClosed(t *testing.T) {
	t.Run("existing before this feature", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token.txt")
		value, err := newTokenValue()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		tok, made, err := LoadToken(path)
		if err != nil || made {
			t.Fatalf("existing load: made=%v err=%v", made, err)
		}
		status, out := claimRequest(t, claimServer(tok).Public(), "device")
		if status != http.StatusConflict || out.Token != "" || !out.Closed || out.At != "" || out.Address != "" {
			t.Fatalf("existing file should be closed without invented assignment metadata: %d %+v", status, withoutClaimToken(out))
		}
	})
	t.Run("restart without having claimed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token.txt")
		if _, _, err := LoadToken(path); err != nil {
			t.Fatal(err)
		}
		tok, made, err := LoadToken(path)
		if err != nil || made {
			t.Fatalf("restart: made=%v err=%v", made, err)
		}
		status, out := claimRequest(t, claimServer(tok).Public(), "device")
		if status != http.StatusConflict || out.Token != "" || !out.Closed {
			t.Fatal("an existing token reopened on restart")
		}
	})
}

func TestFirstTokenClaimCheckOffDoesNotAssign(t *testing.T) {
	tok, _, err := LoadToken(filepath.Join(t.TempDir(), "token.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.SetChecked(false); err != nil {
		t.Fatal(err)
	}
	h := claimServer(tok).Public()
	if rec := send(h, http.MethodGet, "/version", "", ""); rec.Code != http.StatusOK {
		t.Fatal("check off broke normal connection")
	}
	status, out := claimRequest(t, h, "device")
	if status != http.StatusOK || out.Checked || out.Token != "" || out.Closed || out.At != "" || out.Address != "" {
		t.Fatal("check off consumed or disclosed the first token")
	}
	if err := tok.SetChecked(true); err != nil {
		t.Fatal(err)
	}
	status, out = claimRequest(t, h, "device")
	if status != http.StatusOK || out.Token != tok.Value() {
		t.Fatal("check off consumed first assignment")
	}
}

func TestFirstTokenClaimDoesNotChangeOtherRequests(t *testing.T) {
	tok, _, err := LoadToken(filepath.Join(t.TempDir(), "token.txt"))
	if err != nil {
		t.Fatal(err)
	}
	h := claimServer(tok).Public()
	if rec := send(h, http.MethodPost, "/settings/token/claim", "incorrect-synthetic-token", ""); rec.Code != http.StatusUnauthorized || code(t, rec) != CodeTokenInvalid {
		t.Fatal("an incorrect supplied token bypassed the existing token check")
	}
	for _, path := range []string{"/version", "/settings/token", "/settings/token/rotate"} {
		if rec := send(h, http.MethodPost, path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("ordinary request without token passed: %s", path)
		}
	}
	if rec := send(h, http.MethodOptions, "/settings/token/claim", "", ""); rec.Code != http.StatusNoContent {
		t.Fatal("preflight failed")
	}
	if rec := send(h, http.MethodGet, "/settings/token/claim", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatal("non-POST consumed assignment")
	}
	status, out := claimRequest(t, h, "device")
	if status != http.StatusOK || out.Token != tok.Value() {
		t.Fatal("unrelated request consumed assignment")
	}
}
