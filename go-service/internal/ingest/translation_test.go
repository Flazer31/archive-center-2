package ingest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"testing"
)

func gz(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return "z:" + base64.StdEncoding.EncodeToString(b.Bytes())
}

const yumiDisplay = "앞 <!-- yumi-tr:v1:ab12:start -->번역문<!-- yumi-tr:v1:ab12:end --> 뒤"

func TestYumiRecordFormats(t *testing.T) {
	for name, rec := range map[string]string{
		"plain": `{"model":"ORIGINAL"}`,
		"u":     `u:{"model":"ORIGINAL"}`,
		"z":     gz(t, `{"model":"ORIGINAL"}`),
	} {
		got, st := CanonicalOriginal(yumiDisplay, map[string]string{"$__yumi_tr.ab12": rec})
		if st != OriginalYumi || got != "앞 ORIGINAL 뒤" {
			t.Fatalf("%s: got %q %s", name, got, st)
		}
	}
}

// A missing or broken record must not fall back to the translated display.
func TestYumiMissingRecordIsUnavailableNotTranslation(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"missing": {},
		"broken":  {"$__yumi_tr.ab12": "u:{not json"},
		"nomodel": {"$__yumi_tr.ab12": `{"display":"x"}`},
	} {
		got, st := CanonicalOriginal(yumiDisplay, vars)
		if st != OriginalUnavailable || got != "" {
			t.Fatalf("%s: got %q %s", name, got, st)
		}
	}
}

func TestGigaTransBlocks(t *testing.T) {
	got, st := CanonicalOriginal("disp <GigaTrans>원문 1</GigaTrans> x <GigaTrans lang=\"en\">원문 2</GigaTrans>", nil)
	if st != OriginalGigaTrans || got != "원문 1\n\n원문 2" {
		t.Fatalf("got %q %s", got, st)
	}
	if got, st := CanonicalOriginal("<GigaTrans>  </GigaTrans>", nil); st != OriginalUnavailable || got != "" {
		t.Fatalf("empty giga: %q %s", got, st)
	}
}

func TestPlainTextUnchanged(t *testing.T) {
	if got, st := CanonicalOriginal("그냥 응답", nil); st != OriginalPlain || got != "그냥 응답" {
		t.Fatalf("got %q %s", got, st)
	}
}
