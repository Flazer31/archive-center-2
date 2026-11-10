package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
)

// Narrative support (1.0 E03, preprocessing step 4, design
// 20261107-ac2-preprocessing/DESIGN-04.md, user 11-08 "4단계로", "둘 다
// 선택지로", "출판사 연결 따로", "빼기"). Both are optional and off by
// default; any failure sends the request as it would go without them.
const (
	GuideOff    = ""
	GuideWeak   = "weak"   // connections: what of the chosen memory runs into the scene
	GuideMid    = "mid"    // + reactions: each person's likely reaction
	GuideStrong = "strong" // + beats: this reply's opening, development and close
)

// GuideLevels are the levels the settings screen offers, in its order.
var GuideLevels = []string{GuideOff, GuideWeak, GuideMid, GuideStrong}

const (
	InputOff     = ""
	InputClarify = "clarify" // the user's own words made clear; no memory, nothing new
	InputBlend   = "blend"   // related memories woven into the user's own sentences as short phrases
)

// InputModes are the input improvement choices, in the screen's order.
var InputModes = []string{InputOff, InputClarify, InputBlend}

// DefaultGuideChars is the guide's own ceiling until one is saved (AC1
// narrativeSupportMaxChars). A ceiling, never a fill target.
const DefaultGuideChars = 3000

var ErrNarrativeChoice = errors.New("unknown narrative support choice")

// NarrativeSettings are the guide level, its ceiling, the input improvement
// and their AI connection ("the publisher"): the critic's connection unless
// AIOwn (user 11-08 "출판사 연결 따로"). The key is sealed.
type NarrativeSettings struct {
	Level string
	Chars int
	Input string
	AIOwn bool
	AI    provider.Config
}

var narrativeSettingNames = []string{"guide_level", "guide_chars", "guide_input", "guide_ai_own", "guide_ai_provider", "guide_ai_model", "guide_ai_url", "guide_ai_reasoning", "guide_ai_options", "guide_ai_key"}

// NarrativeSettings reads the saved choice; the key is opened with box.
func (d *DB) NarrativeSettings(box *secret.Box) (NarrativeSettings, error) {
	s := NarrativeSettings{Chars: DefaultGuideChars}
	args := make([]any, len(narrativeSettingNames))
	for i, n := range narrativeSettingNames {
		args[i] = n
	}
	rows, err := d.sql.Query(`SELECT name, value FROM settings WHERE name IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return s, err
		}
		switch name {
		case "guide_level":
			s.Level = value
		case "guide_chars":
			if s.Chars, err = strconv.Atoi(value); err != nil {
				return s, err
			}
		case "guide_input":
			s.Input = value
		case "guide_ai_own":
			s.AIOwn = value == "1"
		case "guide_ai_provider":
			s.AI.Provider = value
		case "guide_ai_model":
			s.AI.Model = value
		case "guide_ai_url":
			s.AI.URL = value
		case "guide_ai_reasoning":
			s.AI.Reasoning = value
		case "guide_ai_options":
			if value != "" {
				if err := json.Unmarshal([]byte(value), &s.AI.Options); err != nil {
					return s, err
				}
			}
		case "guide_ai_key":
			if value != "" {
				if s.AI.Key, err = box.Open(value); err != nil {
					return s, err
				}
			}
		}
	}
	return s, rows.Err()
}

func knownIn(v string, list []string) bool {
	for _, x := range list {
		if v == x {
			return true
		}
	}
	return false
}

// SetNarrativeSettings saves the choice at once (all or none); the key is
// sealed.
func (d *DB) SetNarrativeSettings(box *secret.Box, s NarrativeSettings) error {
	if !knownIn(s.Level, GuideLevels) || !knownIn(s.Input, InputModes) {
		return ErrNarrativeChoice
	}
	if s.Chars < 0 {
		return ErrNegativeBudget
	}
	if err := s.AI.Options.Check(); err != nil {
		return err
	}
	key := ""
	if k := strings.TrimSpace(s.AI.Key); k != "" {
		var err error
		if key, err = box.Seal(k); err != nil {
			return err
		}
	}
	options, err := json.Marshal(s.AI.Options)
	if err != nil {
		return err
	}
	own := "0"
	if s.AIOwn {
		own = "1"
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, v := range map[string]string{"guide_level": s.Level, "guide_chars": strconv.Itoa(s.Chars), "guide_input": s.Input, "guide_ai_own": own,
		"guide_ai_provider": s.AI.Provider, "guide_ai_model": strings.TrimSpace(s.AI.Model), "guide_ai_url": strings.TrimSpace(s.AI.URL),
		"guide_ai_reasoning": s.AI.Reasoning, "guide_ai_options": string(options), "guide_ai_key": key} {
		if _, err := tx.Exec(`INSERT INTO settings (name, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, name, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NarrativeRecord is what one delivery's narrative support did, kept with
// the delivery: the guide's level, what it kept and dropped (an item with no
// basis is dropped, user 11-08 "빼기"), the input improvement and its text
// beside the original (shown on the this-turn tab), calls and errors (keys
// masked; status and HUD carry a summary).
type NarrativeRecord struct {
	Level       string `json:"level,omitempty"`
	Guide       string `json:"guide,omitempty"` // "made", or why none was made
	Kept        int    `json:"kept"`            // items put in
	Dropped     int    `json:"dropped"`         // items with no basis
	OverBudget  int    `json:"over_budget"`     // items that did not fit the ceiling
	Chars       int    `json:"chars"`           // the guide's size
	CharsMax    int    `json:"chars_max"`       // its ceiling
	GuideError  string `json:"guide_error,omitempty"`
	Input       string `json:"input,omitempty"`        // the input mode
	InputResult string `json:"input_result,omitempty"` // "changed", "unchanged", or why not
	InputError  string `json:"input_error,omitempty"`
	InputBefore string `json:"input_before,omitempty"`
	InputAfter  string `json:"input_after,omitempty"`
	GuideText   string `json:"guide_text,omitempty"` // what was sent, both parts
	AICalls     int    `json:"ai_calls"`
	MS          int64  `json:"ms"`
}
