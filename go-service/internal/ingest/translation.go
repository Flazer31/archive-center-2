// Package ingest turns plugin observation events into lifecycle snapshots.
package ingest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// OriginalStatus says where a response row's stored text came from.
type OriginalStatus string

const (
	OriginalPlain       OriginalStatus = "plain"       // no translation markers
	OriginalYumi        OriginalStatus = "yumi_model"  // Yumi record .model text
	OriginalGigaTrans   OriginalStatus = "gigatrans"   // <GigaTrans> block text
	OriginalUnavailable OriginalStatus = "unavailable" // markers present, source not readable (yet)
)

// Yumi v1 markers and GigaTrans blocks, following AC1
// buildYumiV1ArchiveReadContext / extractGigaTransCanonicalAssistantText.
var (
	yumiMarker  = regexp.MustCompile(`(?is)<!--\s*yumi-tr:v1:([A-Za-z0-9_-]+):start\s*-->(.*?)<!--\s*yumi-tr:v1:([A-Za-z0-9_-]+):end\s*-->`)
	yumiAny     = regexp.MustCompile(`(?i)<!--\s*yumi-tr:v1:`)
	gigaPresent = regexp.MustCompile(`(?i)<\s*GigaTrans\b`)
	gigaBlock   = regexp.MustCompile(`(?is)<\s*GigaTrans\b[^>]*>(.*?)<\s*/\s*GigaTrans\s*>`)
)

// decodeYumiRecord reads a "$__yumi_tr.<id>" value: plain JSON, "u:" JSON or
// "z:" base64 gzip JSON, and returns its .model text.
func decodeYumiRecord(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	text := raw
	switch {
	case strings.HasPrefix(raw, "u:"):
		text = raw[2:]
	case strings.HasPrefix(raw, "z:"):
		bin, err := base64.StdEncoding.DecodeString(raw[2:])
		if err != nil {
			return "", false
		}
		zr, err := gzip.NewReader(bytes.NewReader(bin))
		if err != nil {
			return "", false
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			return "", false
		}
		text = string(out)
	}
	var rec struct {
		Model *string `json:"model"`
	}
	if json.Unmarshal([]byte(text), &rec) != nil || rec.Model == nil {
		return "", false
	}
	return *rec.Model, true
}

// CanonicalOriginal returns the text to store for an assistant row. A missing
// source is never replaced by the translated display: the status says unavailable.
func CanonicalOriginal(display string, vars map[string]string) (string, OriginalStatus) {
	status := OriginalPlain
	unavailable := false
	out := display
	if yumiAny.MatchString(display) {
		status = OriginalYumi
		out = yumiMarker.ReplaceAllStringFunc(display, func(block string) string {
			m := yumiMarker.FindStringSubmatch(block)
			if len(m) < 4 || !strings.EqualFold(m[1], m[3]) {
				unavailable = true
				return ""
			}
			model, ok := decodeYumiRecord(vars["$__yumi_tr."+m[1]])
			if !ok || strings.TrimSpace(model) == "" {
				unavailable = true
				return ""
			}
			return model
		})
		if yumiAny.MatchString(out) {
			unavailable = true
		}
	}
	if gigaPresent.MatchString(out) {
		blocks := []string{}
		for _, m := range gigaBlock.FindAllStringSubmatch(out, -1) {
			if b := strings.TrimSpace(m[1]); b != "" {
				blocks = append(blocks, b)
			}
		}
		if len(blocks) == 0 {
			unavailable = true
		} else {
			out = strings.TrimSpace(strings.Join(blocks, "\n\n"))
			status = OriginalGigaTrans
		}
	}
	if unavailable {
		return "", OriginalUnavailable
	}
	return out, status
}

// datePattern is a date a chat variable may hold: year, month and day
// (yyyy/mm/dd, yyyy-mm-dd or yyyy.mm.dd), with an optional time. A
// variable is read by its value, never by its name.
var datePattern = regexp.MustCompile(`^\d{1,4}[-/.]\d{1,2}[-/.]\d{1,2}(?:[ T]\d{1,2}:\d{2}(?::\d{2})?)?$`)

// storyDate reports a variable value that is a date (a JSON string value
// is read as its text).
func storyDate(v string) (string, bool) {
	v = strings.TrimSpace(v)
	var s string
	if json.Unmarshal([]byte(v), &s) == nil {
		v = strings.TrimSpace(s)
	}
	return v, datePattern.MatchString(v)
}
