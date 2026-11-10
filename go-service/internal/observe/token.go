package observe

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/secret"
)

// TokenHeader carries the access token on every request. The token is
// checked on every listen address including loopback (user decision
// 10-27) unless the user turns the check off (10-28, on by default).
const TokenHeader = "X-AC2-Token"

// Stable error codes for the token check.
const (
	CodeTokenRequired    = "token_required"
	CodeTokenInvalid     = "token_invalid"
	CodeTokenClaimClosed = "token_claim_closed"
)

// Token is the backend's access token. The backend makes it on first start
// and keeps it in a file the user copies it from; it is never written to the
// log. Rotating replaces it at once for every device.
type Token struct {
	path      string
	mu        sync.RWMutex
	value     string
	created   time.Time
	off       bool // the check is turned off (a file next to the token says so)
	claimOpen bool // only the process that first made token.txt opens assignment
	claim     tokenAssignment
}

type tokenAssignment struct {
	Closed  bool   `json:"closed"`
	At      string `json:"claimed_at,omitempty"`
	Address string `json:"address,omitempty"`
}

func (t *Token) claimPath() string { return t.path + ".claim.json" }

// offPath marks the check turned off; without it the token is checked.
func (t *Token) offPath() string { return t.path + ".off" }

// LoadToken reads the token file, or makes the token and writes the file
// when there is none. created is true when the token was made now.
func LoadToken(path string) (tok *Token, created bool, err error) {
	t := &Token{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		v := strings.TrimSpace(string(b))
		if v == "" {
			return nil, false, errors.New("token file " + path + " is empty")
		}
		// a file made before (or copied in) gets the owner-only access too
		if err := secret.Restrict(path); err != nil {
			return nil, false, err
		}
		st, _ := os.Stat(path)
		t.value = v
		if _, err := os.Stat(t.offPath()); err == nil {
			t.off = true
		}
		if st != nil {
			t.created = st.ModTime()
		}
		// Existing token files always start closed, even if this process
		// stopped before its first assignment. Old files have no metadata.
		if raw, err := os.ReadFile(t.claimPath()); err == nil {
			if err := json.Unmarshal(raw, &t.claim); err != nil {
				log.Printf("first token assignment record: %v", err)
			}
		}
		t.claim.Closed = true
		return t, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	if _, _, err := t.Rotate(); err != nil {
		return nil, false, err
	}
	// A deleted token.txt starts a new token and clears the old assignment.
	if err := secret.WriteFile(t.claimPath(), []byte("{\"closed\":false}\n")); err != nil {
		return nil, false, err
	}
	t.claimOpen = true
	return t, true, nil
}

// FixedToken is a token that lives in memory only (tests).
func FixedToken(v string) *Token { return &Token{value: v, created: time.Now()} }

func newTokenValue() (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return "ac2-" + s[0:6] + "-" + s[6:12] + "-" + s[12:18] + "-" + s[18:24], nil
}

// Rotate makes a new token; the old one stops working at once. The file and
// the token in use change together (rotations run one at a time), and the
// file is readable by the backend's own account only. It returns the new
// token and when it was made.
func (t *Token) Rotate() (string, time.Time, error) {
	v, err := newTokenValue()
	if err != nil {
		return "", time.Time{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path != "" {
		if err := secret.WriteFile(t.path, []byte(v+"\n")); err != nil {
			return "", time.Time{}, err
		}
	}
	t.value, t.created = v, time.Now()
	return t.value, t.created, nil
}

// Value is the current token (handed only to the device that rotated it).
func (t *Token) Value() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.value
}

// Info is what the settings screen shows: the last 4 characters and when
// the token was made.
func (t *Token) Info() (last4 string, created time.Time) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return lastFour(t.value), t.created
}

func lastFour(v string) string {
	if len(v) > 4 {
		return v[len(v)-4:]
	}
	return v
}

// Checked tells whether requests need the token.
func (t *Token) Checked() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return !t.off
}

// SetChecked turns the check on or off; the choice is kept across starts.
func (t *Token) SetChecked(on bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path != "" {
		if on {
			if err := os.Remove(t.offPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err := os.WriteFile(t.offPath(), []byte("off\n"), 0o600); err != nil {
			return err
		}
	}
	t.off = !on
	return nil
}

func (t *Token) match(s string) bool {
	v := t.Value()
	return v != "" && subtle.ConstantTimeCompare([]byte(s), []byte(v)) == 1
}

// requireToken checks the token on every request except the browser's
// CORS preflight, which carries no headers of its own. A server without a
// token answers nothing.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First assignment is the only token-free connection operation.
		// Its availability and persistence are decided by Token, not the host.
		if r.URL.Path == "/settings/token/claim" && s.Token != nil && r.Header.Get(TokenHeader) == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions || (s.Token != nil && !s.Token.Checked()) {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get(TokenHeader)
		if got == "" {
			writeError(w, http.StatusUnauthorized, CodeTokenRequired, "access token required")
			return
		}
		if s.Token == nil || !s.Token.match(got) {
			writeError(w, http.StatusUnauthorized, CodeTokenInvalid, "access token does not match")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenClaim hands the first token to one connecting device. The same lock
// serializes claims, rotations and the check switch. Persist before replying,
// and never write the token into the assignment record or log.
func (s *Server) tokenClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST only")
		return
	}
	t := s.Token
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.off {
		writeJSON(w, http.StatusOK, map[string]any{"checked": false})
		return
	}
	if !t.claimOpen {
		writeJSON(w, http.StatusConflict, map[string]any{"code": CodeTokenClaimClosed, "closed": true, "claimed_at": t.claim.At, "address": t.claim.Address})
		return
	}
	assignment := tokenAssignment{Closed: true, At: s.now().UTC().Format(time.RFC3339), Address: r.RemoteAddr}
	raw, err := json.Marshal(assignment)
	if err == nil {
		err = secret.WriteFile(t.claimPath(), raw)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStoreWriteFailed, err.Error())
		return
	}
	t.claim, t.claimOpen = assignment, false
	log.Printf("첫 토큰이 %s에 할당됨, %s", assignment.Address, assignment.At)
	writeJSON(w, http.StatusOK, map[string]any{"token": t.value, "checked": true, "closed": true, "claimed_at": assignment.At, "address": assignment.Address})
}

// tokenInfo answers GET /settings/token (last 4 characters and the time it
// was made) and POST /settings/token/rotate (a new token; the device that
// asked receives it, every other device must enter it again).
func (s *Server) tokenInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET only")
		return
	}
	last4, created := s.Token.Info()
	writeJSON(w, http.StatusOK, map[string]any{"last4": last4, "created_at": created.UTC().Format(time.RFC3339), "checked": s.Token.Checked()})
}

// tokenCheck answers POST /settings/token/check {"checked": bool}. Turning
// the check on hands the token to the device that asked, which keeps it
// (while the check was off any device could reach the backend anyway).
func (s *Server) tokenCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Checked *bool `json:"checked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Checked == nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "checked required")
		return
	}
	if err := s.Token.SetChecked(*req.Checked); err != nil {
		writeError(w, http.StatusInternalServerError, CodeStoreWriteFailed, err.Error())
		return
	}
	out := map[string]any{"status": "ok", "checked": *req.Checked}
	if *req.Checked {
		last4, _ := s.Token.Info()
		out["token"], out["last4"] = s.Token.Value(), last4
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) tokenRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST only")
		return
	}
	v, created, err := s.Token.Rotate()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStoreWriteFailed, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "token": v, "last4": lastFour(v), "created_at": created.UTC().Format(time.RFC3339)})
}
