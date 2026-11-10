package ingest

import (
	"encoding/json"
	"fmt"

	"archivecenter2/goservice/internal/lifecycle"
)

// Plugin event shapes (plugin/archive-center-2.js, observation.v1).
type textField struct {
	Text *string `json:"text"`
	Len  int     `json:"len"`
	H    string  `json:"h"`
}

type eventRow struct {
	I            int             `json:"i"`
	Role         string          `json:"role"`
	ChatID       *string         `json:"chat_id"`
	Time         *int64          `json:"time"`
	Disabled     json.RawMessage `json:"disabled"` // false | true | "allBefore" | null
	IsComment    *bool           `json:"is_comment"`
	GenerationID *string         `json:"generation_id"`
	Data         textField       `json:"data"`
}

type eventVar struct {
	TranslationRecord bool    `json:"translation_record"`
	Text              *string `json:"text"`
	Len               int     `json:"len"`
	H                 string  `json:"h"`
}

type eventChat struct {
	Missing     bool                `json:"missing"`
	Error       string              `json:"error"`
	ChatID      *string             `json:"chat_id"`
	Name        *string             `json:"name"`      // the chat's name in the host
	CharName    *string             `json:"char_name"` // its character's name, when the plugin read it
	Rows        []eventRow          `json:"rows"`
	ScriptState map[string]eventVar `json:"scriptstate"`
}

type eventCoords struct {
	CharacterIndex *int `json:"character_index"`
	ChatIndex      *int `json:"chat_index"`
	MessageIndex   *int `json:"message_index"`
}

type event struct {
	BootID     string      `json:"boot_id"`
	Seq        int         `json:"seq"`
	Hook       string      `json:"hook"`
	Coords     eventCoords `json:"coords"`
	Listener   eventCoords `json:"listener"`
	StoredChat *eventChat  `json:"stored_chat"`
	EventChat  *eventChat  `json:"event_chat"`
	RereadChat *eventChat  `json:"reread_chat"`
}

// RowNote records how a row's text was obtained. A row whose text could not
// be read is still present in the host chat; it is not a deletion.
type RowNote struct {
	Index  int
	Status OriginalStatus // plain / yumi_model / gigatrans / unavailable
	Reason string         // "translation_source_missing" | "text_not_captured"
}

// Result is one snapshot built from one chat in an event.
type Result struct {
	Snapshot lifecycle.Snapshot
	Notes    []RowNote
	// NeedText: a text came as fingerprint only and is not in the cache (for
	// example after a backend restart); the plugin should resend texts.
	NeedText bool
	// ChatName and CharName name the chat for the chat list ("" = not sent)
	ChatName, CharName string
}

// FromEvent turns one stored observation event into lifecycle snapshots.
// beforeRequest and load give the stored chat; output gives the listener's event
// chat and the later re-read as two separate snapshots. Other hooks carry
// no chat and give none.
//
// cache fills texts the plugin sent as fingerprints; nil means no cache.
func FromEvent(raw json.RawMessage, cache *TextCache) ([]Result, error) {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	var out []Result
	add := func(c *eventChat, src lifecycle.Source, co eventCoords) {
		if c == nil || c.Missing || c.Error != "" {
			return
		}
		out = append(out, buildSnapshot(c, src, co, cache))
	}
	switch ev.Hook {
	case "beforeRequest":
		add(ev.StoredChat, lifecycle.SourceBeforeRequest, ev.Coords)
	case "load":
		add(ev.StoredChat, lifecycle.SourceLoad, ev.Coords)
	case "output":
		add(ev.EventChat, lifecycle.SourceOutputEvent, ev.Listener)
		add(ev.RereadChat, lifecycle.SourceOutputReread, ev.Listener)
	}
	return out, nil
}

// sessionKey uses the host chat.id; without it, the coordinates (unverified:
// coordinates can point at a different chat after the user switches).
func sessionKey(c *eventChat, co eventCoords) string {
	if c.ChatID != nil && *c.ChatID != "" {
		return "chat:" + *c.ChatID
	}
	ch, ct := -1, -1
	if co.CharacterIndex != nil {
		ch = *co.CharacterIndex
	}
	if co.ChatIndex != nil {
		ct = *co.ChatIndex
	}
	return fmt.Sprintf("coord:%d/%d", ch, ct)
}

func disabledValue(raw json.RawMessage) string {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return "true"
		}
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// textKey is the plugin's text key of a row.
func textKey(chatKey string, r eventRow) TextKey {
	if r.ChatID != nil && *r.ChatID != "" {
		return TextKey{chatKey, "c:" + *r.ChatID}
	}
	return TextKey{chatKey, fmt.Sprintf("i:%d", r.I)}
}

func chatKeyOf(c *eventChat) string {
	if c.ChatID != nil {
		return *c.ChatID
	}
	return ""
}

// ChatsOf returns the distinct plugin chat ids of an event's chat snapshots
// ("" for a chat without id), in event order. An output event's own chat and
// its re-read can be different chats.
func ChatsOf(raw json.RawMessage) []string {
	var ev event
	if json.Unmarshal(raw, &ev) != nil {
		return nil
	}
	var out []string
	for _, c := range []*eventChat{ev.StoredChat, ev.EventChat, ev.RereadChat} {
		if c == nil || c.Missing || c.Error != "" {
			continue
		}
		id := chatKeyOf(c)
		if len(out) == 0 || out[0] != id {
			out = append(out, id)
		}
	}
	return out
}

// LearnEvent adds every text a stored event carries to the cache, so texts
// the plugin sent before a restart fill its later fingerprints exactly
// (display texts, translation records, rows without chatId).
func (c *TextCache) LearnEvent(raw json.RawMessage) {
	var ev event
	if c == nil || json.Unmarshal(raw, &ev) != nil {
		return
	}
	for _, ch := range []*eventChat{ev.StoredChat, ev.EventChat, ev.RereadChat} {
		if ch == nil || ch.Missing || ch.Error != "" {
			continue
		}
		key := chatKeyOf(ch)
		for k, v := range ch.ScriptState {
			if v.Text != nil {
				c.add(TextKey{key, "v:" + k}, *v.Text)
			}
		}
		for _, r := range ch.Rows {
			if r.Data.Text != nil {
				c.add(textKey(key, r), *r.Data.Text)
			}
		}
	}
}

func buildSnapshot(c *eventChat, src lifecycle.Source, co eventCoords, cache *TextCache) Result {
	res := Result{Snapshot: lifecycle.Snapshot{SessionKey: sessionKey(c, co), Source: src, TargetIndex: -1}}
	if c.Name != nil {
		res.ChatName = *c.Name
	}
	if c.CharName != nil {
		res.CharName = *c.CharName
	}
	chatKey := chatKeyOf(c)
	vars := map[string]string{}
	for k, v := range c.ScriptState {
		t, ok := cache.resolve(TextKey{chatKey, "v:" + k}, textField{Text: v.Text, Len: v.Len, H: v.H})
		switch {
		case ok:
			vars[k] = t
			if d, isDate := storyDate(t); isDate {
				if res.Snapshot.Dates == nil {
					res.Snapshot.Dates = map[string]string{}
				}
				res.Snapshot.Dates[k] = d
			}
		case v.TranslationRecord:
			// a translation original is needed to store the reply; another
			// variable unknown here only leaves its date unread
			res.NeedText = true
		}
	}
	if co.MessageIndex != nil {
		res.Snapshot.TargetIndex = *co.MessageIndex
	}
	for _, r := range c.Rows {
		row := lifecycle.Row{Index: r.I, Role: r.Role, Disabled: disabledValue(r.Disabled)}
		if r.ChatID != nil {
			row.ChatID = *r.ChatID
		}
		if r.Time != nil {
			row.Time = *r.Time
		}
		if r.IsComment != nil {
			row.IsComment = *r.IsComment
		}
		if r.GenerationID != nil {
			row.GenerationID = *r.GenerationID
		}
		data, known := cache.resolve(textKey(chatKey, r), r.Data)
		switch {
		case !known:
			row.Unreadable = true
			res.NeedText = res.NeedText || r.Data.H != ""
			res.Notes = append(res.Notes, RowNote{Index: r.I, Status: OriginalUnavailable, Reason: "text_not_captured"})
		case r.Role == "char":
			text, st := CanonicalOriginal(data, vars)
			row.Data = text
			note := RowNote{Index: r.I, Status: st}
			if st == OriginalUnavailable {
				row.Unreadable = true
				note.Reason = "translation_source_missing"
			}
			res.Notes = append(res.Notes, note)
		default:
			row.Data = data
			res.Notes = append(res.Notes, RowNote{Index: r.I, Status: OriginalPlain})
		}
		res.Snapshot.Rows = append(res.Snapshot.Rows, row)
	}
	return res
}
