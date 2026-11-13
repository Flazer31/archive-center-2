package ingest

import (
	"fmt"
	"unicode/utf16"

	"archivecenter2/goservice/internal/lifecycle"
)

// TextCache keeps the texts received for each chat row and translation
// record. The plugin sends a text only when it differs from the one the
// backend last acknowledged and otherwise only its fingerprint; the cache
// fills the text back in from the row's texts by fingerprint, so a return to
// an earlier candidate is filled too. A key is the plugin chat id ("" without
// id) and the row: "c:<row chatId>", "i:<row index>" or "v:<variable name>".
type TextCache struct {
	texts map[TextKey][]cachedText // key -> distinct texts received or stored
	// Load, when set, warms the cache with a chat's stored texts the first
	// time a fingerprint of that chat is not found (after a restart); the
	// chat id is "" for a chat without id.
	Load   func(chat string) bool // false: not loaded (tried again later)
	loaded map[string]bool
	// dropped are the fingerprints of bodies a reroll or an edit replaced,
	// per chat (not the texts): not learnt again from a resent batch or a
	// stored observation until the chat holds the text again
	dropped map[string]map[string]bool
}

// TextKey names one row (or translation record) of one chat.
type TextKey struct{ Chat, Row string }

type cachedText struct {
	text string
	len  int
	h    string
}

func NewTextCache() *TextCache {
	return &TextCache{texts: map[TextKey][]cachedText{}, loaded: map[string]bool{}}
}

// Fingerprint is the plugin's fingerprint: length in UTF-16 code units and
// djb2 + FNV-1a over them (16 hex digits).
func Fingerprint(s string) (int, string) {
	units := utf16.Encode([]rune(s))
	var a, b uint32 = 5381, 0x811c9dc5
	for _, u := range units {
		a = a*33 + uint32(u)
		b = (b ^ uint32(u)) * 0x01000193
	}
	return len(units), fmt.Sprintf("%08x%08x", a, b)
}

// Warm adds a text known from storage (for example after a restart): one
// the chat holds now, also when it was dropped before.
func (c *TextCache) Warm(key TextKey, text string) {
	if c != nil {
		delete(c.dropped[key.Chat], mark(text))
		c.add(key, text)
	}
}

// mark is the fingerprint a dropped body is remembered by.
func mark(text string) string {
	n, h := Fingerprint(text)
	return fmt.Sprintf("%d:%s", n, h)
}

// ForgetText drops a body a reroll or an edit replaced from every row of a
// chat, with any display of it (lifecycle.ShowsDropped), and keeps it from
// being learnt again. held is what the chat holds now.
func (c *TextCache) ForgetText(chat, text string, held map[string]bool) {
	if c == nil {
		return
	}
	if c.dropped == nil {
		c.dropped = map[string]map[string]bool{}
	}
	if c.dropped[chat] == nil {
		c.dropped[chat] = map[string]bool{}
	}
	c.dropped[chat][mark(text)] = true
	for key, list := range c.texts {
		if key.Chat != chat {
			continue
		}
		var kept []cachedText
		for _, t := range list {
			if !lifecycle.ShowsDropped(t.text, text, held) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(c.texts, key)
		} else {
			c.texts[key] = kept
		}
	}
}

// Held lifts the drop of the bodies the chat holds again (a swipe back, the
// same words written in a later turn), so they are learnt again.
func (c *TextCache) Held(chat string, held map[string]bool) {
	if c == nil || len(c.dropped[chat]) == 0 {
		return
	}
	for t := range held {
		delete(c.dropped[chat], mark(t))
	}
}

func (c *TextCache) add(key TextKey, text string) {
	if len(c.dropped[key.Chat]) > 0 && c.dropped[key.Chat][mark(text)] {
		return
	}
	for _, t := range c.texts[key] {
		if t.text == text {
			return
		}
	}
	n, h := Fingerprint(text)
	c.texts[key] = append(c.texts[key], cachedText{text: text, len: n, h: h})
}

// resolve returns the text for a field: the sent text (remembered), or the
// one remembered text with that fingerprint. ok is false when no text, or
// more than one, matches; the caller then asks the plugin to resend.
func (c *TextCache) resolve(key TextKey, f textField) (string, bool) {
	if f.Text != nil {
		if c != nil {
			c.add(key, *f.Text)
		}
		return *f.Text, true
	}
	if c == nil {
		return "", false
	}
	t, ok := c.match(key, f)
	if !ok && c.Load != nil {
		if !c.loaded[key.Chat] && c.Load(key.Chat) {
			c.loaded[key.Chat] = true
			t, ok = c.match(key, f)
		}
	}
	return t, ok
}

func (c *TextCache) match(key TextKey, f textField) (string, bool) {
	found, n := "", 0
	for _, t := range c.texts[key] {
		if t.len == f.Len && t.h == f.H {
			found = t.text
			n++
		}
	}
	return found, n == 1
}
