package memory

import "strings"

// PayloadMessage is a transient Host observation used only to place memory:
// a message's role, its cache flags and whether it holds the anchor marker.
// The prompt text itself is never sent (it stays fingerprints only).
type PayloadMessage struct {
	Role         string `json:"role"`
	Marker       bool   `json:"marker"` // holds the anchor marker the plugin was told
	CachePoint   any    `json:"cachePoint,omitempty"`
	CacheControl any    `json:"cache_control,omitempty"`
	IsCache      any    `json:"isCache,omitempty"`
}

type Placement struct {
	Index     int    `json:"index"`
	Requested string `json:"requested"`
	Resolved  string `json:"resolved"`
}

var PlacementModes = []string{"auto", "before_latest_user", "after_anchor_marker", "after_last_cache_point", "after_first_system", "end"}

func PlacementMode(mode string) string {
	for _, m := range PlacementModes {
		if mode == m {
			return m
		}
	}
	return "auto"
}

// ResolvePlacement follows AC1's whole-message anchor/cache placement and
// explicit-mode fallback order. Auto without an anchor retains AC2's previous
// latest-user (or end when no user) placement.
func ResolvePlacement(messages []PayloadMessage, mode, marker string) Placement {
	mode = PlacementMode(mode)
	latest, first, anchor, cache := -1, -1, -1, -1
	marker = strings.TrimSpace(marker)
	for i, m := range messages {
		switch strings.ToLower(m.Role) {
		case "user":
			latest = i
		case "system":
			if first < 0 {
				first = i
			}
		}
		if marker != "" && m.Marker {
			anchor = i + 1
		}
		if cacheFlag(m.CachePoint) || cacheFlag(m.CacheControl) || cacheFlag(m.IsCache) {
			cache = i + 1
		}
	}
	result := func(index int, resolved string) Placement { return Placement{index, mode, resolved} }
	switch mode {
	case "end":
		return result(len(messages), mode)
	case "after_first_system":
		return result(first+1, mode)
	case "after_anchor_marker":
		if anchor >= 0 {
			return result(anchor, mode)
		}
	case "after_last_cache_point":
		if cache >= 0 {
			return result(cache, mode)
		}
	case "auto":
		if anchor >= 0 {
			return result(anchor, "after_anchor_marker")
		}
	}
	if latest >= 0 {
		return result(latest, "before_latest_user")
	}
	if mode == "auto" {
		return result(len(messages), "end")
	}
	return result(first+1, "after_first_system")
}

// AC1 tests these optional Host fields for truthiness. Missing fields remain
// unobserved; no cache boundary is invented from prompt prose or structure.
func cacheFlag(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	default:
		return true
	}
}
