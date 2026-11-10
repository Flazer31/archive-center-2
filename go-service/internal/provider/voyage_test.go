package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestContextual(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"voyage-context-4", true},
		{"  VoYaGe-CoNtExT-4 \t", true},
		{"voyage-4-large", false},
		{"", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := Contextual(tc.model); got != tc.want {
				t.Fatalf("Contextual(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

type voyageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f voyageRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVoyageEmbedGroupsRequestAndOrdering(t *testing.T) {
	groups := [][]string{{"first chunk", "second chunk"}, {"other context"}}
	wantVectors := [][][]float32{{{1, 2}, {3, 4}}, {{5, 6}}}
	for _, tc := range []struct {
		name, endpoint, path, inputType string
	}{
		{"default", "", "/v1/contextualizedembeddings", "document"},
		{"embeddings_suffix", "/v1/embeddings", "/v1/contextualizedembeddings", "query"},
		{"contextual_endpoint", "/v1/contextualizedembeddings", "/v1/contextualizedembeddings", "document"},
		{"custom_base", "/custom", "/custom/contextualizedembeddings", "query"}, // AC1 completes a base
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != tc.path {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, tc.path)
				}
				if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
					t.Error("missing JSON content type or synthetic bearer authorization")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				var model, inputType string
				var inputs [][]string
				if err := json.Unmarshal(body["model"], &model); err != nil {
					t.Errorf("model: %v", err)
				}
				if err := json.Unmarshal(body["input_type"], &inputType); err != nil {
					t.Errorf("input_type: %v", err)
				}
				if err := json.Unmarshal(body["inputs"], &inputs); err != nil {
					t.Errorf("inputs: %v", err)
				}
				if len(body) != 3 || model != "voyage-context-4" || inputType != tc.inputType || !reflect.DeepEqual(inputs, groups) {
					t.Errorf("unexpected body: %s", body)
				}
				io.WriteString(w, `{"data":[{"index":1,"data":[{"index":0,"embedding":[5,6]}]},{"index":0,"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}]}],"usage":{"total_tokens":73}}`)
			}))
			defer s.Close()
			v := Voyage{Model: "voyage-context-4", Key: "synthetic-key", HTTP: s.Client()}
			if tc.endpoint == "" {
				// Check the real default URL, then route only its network boundary
				// to the local HTTP server. No external request is made.
				target, err := url.Parse(s.URL)
				if err != nil {
					t.Fatal(err)
				}
				transport := v.HTTP.Transport
				v.HTTP = &http.Client{Transport: voyageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() != "https://api.voyageai.com/v1/contextualizedembeddings" {
						t.Errorf("default URL = %s", r.URL)
					}
					local := r.Clone(r.Context())
					local.URL.Scheme, local.URL.Host = target.Scheme, target.Host
					return transport.RoundTrip(local)
				})}
			} else {
				v.URL = s.URL + tc.endpoint
			}
			got, err := v.EmbedGroups(context.Background(), groups, tc.inputType)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !reflect.DeepEqual(got.Vectors, wantVectors) || got.Tokens != 73 {
				t.Fatalf("calls=%d, result=%+v, want vectors=%v tokens=73", calls, got, wantVectors)
			}
		})
	}
}

func TestVoyageEmbedGroupsInvalidResponse(t *testing.T) {
	groups := [][]string{{"one", "two"}, {"three"}}
	valid := `{"data":[{"index":0,"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2]}]},{"index":1,"data":[{"index":0,"embedding":[3]}]}]}`
	for _, tc := range []struct {
		name, body, wantError string
		status                int
	}{
		{"group_count_short", `{"data":[]}`, "groups for", 200},
		{"group_count_long", `{"data":[{},{},{}]}`, "groups for", 200},
		{"group_index_missing", strings.Replace(valid, `"index":0,`, "", 1), "missing group index", 200},
		{"group_index_null", strings.Replace(valid, `"index":0,`, `"index":null,`, 1), "missing group index", 200},
		{"group_index_negative", strings.Replace(valid, `"index":0,`, `"index":-1,`, 1), "bad group index", 200},
		{"group_index_outside", strings.Replace(valid, `"index":0,`, `"index":2,`, 1), "bad group index", 200},
		{"group_index_duplicate", strings.Replace(valid, `{"index":1,"data":`, `{"index":0,"data":`, 1), "bad group index", 200},
		{"chunk_count_short", strings.Replace(valid, `,{"index":1,"embedding":[2]}`, "", 1), "vectors for", 200},
		{"chunk_count_long", strings.Replace(valid, `{"index":1,"embedding":[2]}`, `{"index":1,"embedding":[2]},{"index":2,"embedding":[4]}`, 1), "vectors for", 200},
		{"chunk_index_missing", strings.Replace(valid, `{"index":0,"embedding":[1]}`, `{"embedding":[1]}`, 1), "missing chunk index", 200},
		{"chunk_index_null", strings.Replace(valid, `{"index":0,"embedding":[1]}`, `{"index":null,"embedding":[1]}`, 1), "missing chunk index", 200},
		{"chunk_index_negative", strings.Replace(valid, `{"index":0,"embedding":[1]}`, `{"index":-1,"embedding":[1]}`, 1), "bad chunk index", 200},
		{"chunk_index_outside", strings.Replace(valid, `{"index":0,"embedding":[1]}`, `{"index":2,"embedding":[1]}`, 1), "bad chunk index", 200},
		{"chunk_index_duplicate", strings.Replace(valid, `{"index":1,"embedding":[2]}`, `{"index":0,"embedding":[2]}`, 1), "bad chunk index", 200},
		{"malformed_json", `{`, "voyage:", 200},
		{"http_error", valid, "voyage 503:", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/contextualizedembeddings" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer s.Close()
			v := Voyage{URL: s.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "synthetic-key", HTTP: s.Client()}
			got, err := v.EmbedGroups(context.Background(), groups, "document")
			if calls != 1 || err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("calls=%d, result=%+v, err=%v, want error containing %q", calls, got, err, tc.wantError)
			}
		})
	}
}

// Queries go as one single-chunk group each ([[q1],[q2]], the documented
// query shape), and come back in query order (ASTRA-RECHECK-71 3).
func TestVoyageEmbedGroupsQueries(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Inputs    [][]string `json:"inputs"`
			InputType string     `json:"input_type"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.InputType != "query" || !reflect.DeepEqual(req.Inputs, [][]string{{"어제 주막"}, {"빚 30냥"}}) {
			t.Errorf("request %+v", req)
		}
		io.WriteString(w, `{"data":[{"index":1,"data":[{"index":0,"embedding":[3,4]}]},{"index":0,"data":[{"index":0,"embedding":[1,2]}]}],"usage":{"total_tokens":9}}`)
	}))
	defer s.Close()
	v := Voyage{URL: s.URL + "/v1/embeddings", Model: "voyage-context-4", Key: "synthetic-key", HTTP: s.Client()}
	got, err := v.EmbedGroups(context.Background(), [][]string{{"어제 주막"}, {"빚 30냥"}}, "query")
	if err != nil || !reflect.DeepEqual(got.Vectors, [][][]float32{{{1, 2}}, {{3, 4}}}) || got.Tokens != 9 {
		t.Fatalf("got %+v %v", got, err)
	}
}
