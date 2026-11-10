package observe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func send(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every path, the health check and added routes included, needs the token;
// a missing and a wrong token get different stable codes.
func TestPublicChecksTokenOnEveryPath(t *testing.T) {
	srv := NewServer(&memSink{})
	reached := 0
	srv.Routes = map[string]http.HandlerFunc{"/memory/inject": func(w http.ResponseWriter, r *http.Request) { reached++; writeJSON(w, 200, map[string]any{}) }}
	srv.Token = FixedToken("ac2-right")
	h := srv.Public()
	obs := `{"contract":"observation.v1","events":[{"hook":"input"}]}`
	for _, c := range []struct{ method, path, body string }{{"GET", "/health", ""}, {"GET", "/version", ""}, {"POST", "/observe", obs}, {"POST", "/memory/inject", "{}"}, {"GET", "/settings/token", ""}} {
		if rec := send(h, c.method, c.path, "", c.body); rec.Code != 401 || code(t, rec) != CodeTokenRequired {
			t.Fatalf("%s without token: %d %s", c.path, rec.Code, rec.Body.String())
		}
		if rec := send(h, c.method, c.path, "ac2-wrong", c.body); rec.Code != 401 || code(t, rec) != CodeTokenInvalid {
			t.Fatalf("%s wrong token: %d %s", c.path, rec.Code, rec.Body.String())
		}
		if rec := send(h, c.method, c.path, "ac2-right", c.body); rec.Code != 200 {
			t.Fatalf("%s right token: %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
	if reached != 1 {
		t.Fatalf("added route reached %d times, want only with the right token", reached)
	}
	if len(srv.sink.(*memSink).records) != 1 {
		t.Fatalf("observations stored without the right token")
	}
}

// The browser preflight carries no token and must pass, and it must allow
// the token header; a server without a token answers nothing.
func TestPreflightAndServerWithoutToken(t *testing.T) {
	srv := NewServer(&memSink{})
	srv.Token = FixedToken("ac2-right")
	rec := send(srv.Public(), "OPTIONS", "/observe", "", "")
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), TokenHeader) {
		t.Fatalf("preflight: %d %q", rec.Code, rec.Header().Get("Access-Control-Allow-Headers"))
	}
	bare := NewServer(&memSink{}).Public()
	if rec := send(bare, "GET", "/health", "anything", ""); rec.Code != 401 || code(t, rec) != CodeTokenInvalid {
		t.Fatalf("server without token answered: %d", rec.Code)
	}
}

// The token is made once, kept in the file and read back on the next start.
func TestLoadTokenMakesOnceAndKeeps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "token.txt")
	a, made, err := LoadToken(path)
	if err != nil || !made {
		t.Fatalf("first load: made=%v err=%v", made, err)
	}
	if !strings.HasPrefix(a.Value(), "ac2-") || len(a.Value()) != 31 {
		t.Fatalf("token shape %q", a.Value())
	}
	b, made2, err := LoadToken(path)
	if err != nil || made2 || b.Value() != a.Value() {
		t.Fatalf("second load: made=%v same=%v err=%v", made2, b.Value() == a.Value(), err)
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != a.Value() {
		t.Fatalf("file holds %q", raw)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadToken(path); err == nil {
		t.Fatalf("an empty token file must not start the backend")
	}
}

// Rotations at the same time (two devices, or a double press) leave the
// file and the token in use the same, so the file's token always connects
// and survives a restart; no temporary file is left behind.
func TestConcurrentRotationsKeepFileAndTokenTogether(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.txt")
	tok, _, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&memSink{})
	srv.Token = tok
	h := srv.Public()
	for round := 0; round < 5; round++ {
		done := make(chan error, 16)
		for i := 0; i < 16; i++ {
			go func() { _, _, err := tok.Rotate(); done <- err }()
		}
		for i := 0; i < 16; i++ {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := os.ReadFile(path)
		file := strings.TrimSpace(string(raw))
		if file != tok.Value() {
			t.Fatalf("round %d: file and token in use differ", round)
		}
		if rec := send(h, "GET", "/health", file, ""); rec.Code != 200 {
			t.Fatalf("round %d: the file's token is refused: %d", round, rec.Code)
		}
		again, _, err := LoadToken(path)
		if err != nil || again.Value() != file {
			t.Fatalf("round %d: restart reads another token", round)
		}
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".secret-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// Rotating stops the old token at once; the device that asked gets the new
// one, the file holds it, and the settings view shows only its end.
func TestRotateReplacesTokenEverywhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.txt")
	tok, _, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	old := tok.Value()
	srv := NewServer(&memSink{})
	srv.Token = tok
	h := srv.Public()
	rec := send(h, "POST", "/settings/token/rotate", old, "")
	if rec.Code != 200 {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	nu := out["token"]
	if nu == "" || nu == old || out["last4"] != nu[len(nu)-4:] {
		t.Fatalf("rotate answer %v", out)
	}
	if rec := send(h, "GET", "/health", old, ""); rec.Code != 401 {
		t.Fatalf("old token still works: %d", rec.Code)
	}
	if rec := send(h, "GET", "/health", nu, ""); rec.Code != 200 {
		t.Fatalf("new token refused: %d", rec.Code)
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != nu {
		t.Fatalf("file not updated")
	}
	info := send(h, "GET", "/settings/token", nu, "")
	if strings.Contains(info.Body.String(), nu) || !strings.Contains(info.Body.String(), nu[len(nu)-4:]) {
		t.Fatalf("settings view must show only the end: %s", info.Body.String())
	}
}

// The check can be turned off and on again, and the choice is kept across
// starts; turned off, requests pass without a token, and turning it on
// hands the token to the device that asked.
func TestTokenCheckSwitch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.txt")
	tok, _, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	v := tok.Value()
	srv := NewServer(&memSink{})
	srv.Token = tok
	h := srv.Public()
	if rec := send(h, "POST", "/settings/token/check", "", `{"checked":false}`); rec.Code != 401 {
		t.Fatalf("turned off without the token: %d", rec.Code)
	}
	if rec := send(h, "POST", "/settings/token/check", v, `{}`); rec.Code != 400 {
		t.Fatalf("no choice: %d", rec.Code)
	}
	if rec := send(h, "POST", "/settings/token/check", v, `{"checked":false}`); rec.Code != 200 || strings.Contains(rec.Body.String(), v) {
		t.Fatalf("turn off: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(h, "GET", "/health", "", ""); rec.Code != 200 {
		t.Fatalf("check off, no token: %d", rec.Code)
	}
	if rec := send(h, "GET", "/settings/token", "ac2-anything", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"checked":false`) {
		t.Fatalf("check off, view: %d %s", rec.Code, rec.Body.String())
	}
	// kept across a start
	again, _, err := LoadToken(path)
	if err != nil || again.Checked() || again.Value() != v {
		t.Fatalf("reloaded: checked %v err %v", again.Checked(), err)
	}
	rec := send(h, "POST", "/settings/token/check", "", `{"checked":true}`)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out["token"] != v {
		t.Fatalf("turn on: %d %v", rec.Code, out)
	}
	if rec := send(h, "GET", "/health", "", ""); rec.Code != 401 {
		t.Fatalf("check on, no token: %d", rec.Code)
	}
	if again, _, _ := LoadToken(path); !again.Checked() {
		t.Fatal("turned on, but off after a start")
	}
}
