package provider

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GitHub Copilot: the user's GitHub token is exchanged for a Copilot token
// on each call, with the editor headers Copilot expects (AC1
// proxyGetCopilotToken). When the exchange is refused the GitHub token is
// used as it is.
var (
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	copilotHeaders  = map[string]string{
		"Editor-Version":         "vscode/1.85.0",
		"Editor-Plugin-Version":  "copilot-chat/0.22.0",
		"Copilot-Integration-Id": "vscode-chat",
		"User-Agent":             "GitHubCopilotChat/0.22.0",
		"X-Github-Api-Version":   "2025-10-01",
		"X-Initiator":            "user",
	}
)

func copilotToken(ctx context.Context, githubToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+githubToken)
	for _, k := range []string{"Editor-Version", "Editor-Plugin-Version", "Copilot-Integration-Id", "User-Agent"} {
		req.Header.Set(k, copilotHeaders[k])
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return "", errors.New(scrub(err.Error(), githubToken))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		Token string `json:"token"`
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || json.Unmarshal(raw, &out) != nil || strings.TrimSpace(out.Token) == "" {
		return githubToken, nil
	}
	return out.Token, nil
}

// Vertex AI: the key is a service account JSON; a signed assertion buys an
// access token, kept until shortly before it ends (AC1
// proxyGetVertexAccessToken). A PROJECT_ID in the address is the account's
// project.
var vertexTokenURL = "https://oauth2.googleapis.com/token"

type vertexChat struct {
	c     Config
	mu    sync.Mutex
	token string
	until time.Time
}

type serviceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	ProjectID   string `json:"project_id"`
}

func (v *vertexChat) Chat(ctx context.Context, prompt string) (Result, error) {
	var sa serviceAccount
	if err := json.Unmarshal([]byte(v.c.Key), &sa); err != nil || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return Result{}, errors.New("vertex: the key must be a service account JSON with client_email and private_key")
	}
	base := v.c.URL
	if strings.Contains(base, "PROJECT_ID") {
		if sa.ProjectID == "" {
			return Result{}, errors.New("vertex: the address has PROJECT_ID but the service account has no project_id")
		}
		base = strings.ReplaceAll(base, "PROJECT_ID", url.PathEscape(sa.ProjectID))
	}
	tok, err := v.accessToken(ctx, sa)
	if err != nil {
		return Result{}, err
	}
	return geminiChat{c: v.c, endpoint: vertexEndpoint(base, v.c.Model), bearer: tok}.Chat(ctx, prompt)
}

// vertexEndpoint completes a Vertex address as AC1 does
// (proxyNormalizeVertexEndpoint): a full :generateContent address is kept
// (a streaming one made plain); a models base takes /<model>:generateContent.
func vertexEndpoint(base, model string) string {
	if strings.Contains(base, ":streamGenerateContent") {
		return strings.Replace(base, ":streamGenerateContent", ":generateContent", 1)
	}
	if strings.Contains(base, ":generateContent") {
		return base
	}
	return base + "/" + model + ":generateContent"
}

func (v *vertexChat) accessToken(ctx context.Context, sa serviceAccount) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && time.Now().Before(v.until) {
		return v.token, nil
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", errors.New("vertex: private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
			parsed = k
		} else {
			return "", errors.New("vertex: private_key cannot be read")
		}
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("vertex: private_key is not RSA")
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now().Unix()
	input := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": sa.ClientEmail, "scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud": "https://oauth2.googleapis.com/token", "iat": now, "exp": now + 3600,
	})
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {input + "." + base64.RawURLEncoding.EncodeToString(sig)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vertexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 || json.Unmarshal(raw, &out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("vertex token %d", res.StatusCode)
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	v.token, v.until = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn-120)*time.Second)
	return v.token, nil
}
