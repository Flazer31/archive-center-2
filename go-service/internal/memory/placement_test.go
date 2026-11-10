package memory

import "testing"

func TestU5Placement(t *testing.T) {
	msgs := []PayloadMessage{{Role: "system", Marker: true}, {Role: "user"}, {Role: "assistant", CachePoint: true}, {Role: "system", Marker: true}, {Role: "user"}, {Role: "assistant"}}
	for _, tc := range []struct {
		mode  string
		index int
	}{{"auto", 4}, {"", 4}, {"before_latest_user", 4}, {"after_anchor_marker", 4}, {"after_last_cache_point", 3}, {"after_first_system", 1}, {"end", 6}} {
		if got := ResolvePlacement(msgs, tc.mode, "<anchor>"); got.Index != tc.index {
			t.Errorf("%q: %+v", tc.mode, got)
		}
	}
	for _, mode := range PlacementModes {
		t.Run(mode, func(t *testing.T) {
			for _, messages := range [][]PayloadMessage{nil, {{Role: "system"}, {Role: "assistant"}}, {{Role: "user"}, {Role: "system"}, {Role: "assistant"}}} {
				got := ResolvePlacement(messages, mode, "absent")
				expected := 0
				switch mode {
				case "end":
					expected = len(messages)
				case "after_first_system":
					for i, m := range messages {
						if m.Role == "system" {
							expected = i + 1
							break
						}
					}
				case "auto":
					expected = len(messages)
					for i, m := range messages {
						if m.Role == "user" {
							expected = i
						}
					}
				default:
					found := false
					for i, m := range messages {
						if m.Role == "user" {
							expected = i
							found = true
						}
					}
					if !found {
						for i, m := range messages {
							if m.Role == "system" {
								expected = i + 1
								break
							}
						}
					}
				}
				if got.Index != expected {
					t.Fatalf("missing target: %+v messages=%+v expected %d", got, messages, expected)
				}
			}
		})
	}
}

func TestU5CacheObservations(t *testing.T) {
	for _, m := range []PayloadMessage{{CachePoint: true}, {CacheControl: map[string]any{"type": "ephemeral"}}, {IsCache: true}} {
		messages := []PayloadMessage{{Role: "system"}, m, {Role: "assistant"}}
		if got := ResolvePlacement(messages, "after_last_cache_point", ""); got.Index != 2 || got.Resolved != "after_last_cache_point" {
			t.Fatal("lost observed cache point")
		}
	}
	for _, m := range []PayloadMessage{{}, {CachePoint: false}, {IsCache: float64(0)}, {CacheControl: ""}} {
		messages := []PayloadMessage{{Role: "system"}, m, {Role: "assistant"}}
		if got := ResolvePlacement(messages, "after_last_cache_point", ""); got.Index != 1 || got.Resolved != "after_first_system" {
			t.Fatal("invented cache point")
		}
	}
}
