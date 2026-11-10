package memory

import (
	"regexp"
	"strings"
)

// Reasoning envelopes are a model's own thinking written into its reply
// (<Thoughts>…</Thoughts>, <think>, <reasoning>, …). Generation reads the
// turn without them (user 10-29: ignored only when memory is made; the
// stored reply keeps them). The names follow AC1
// stripHiddenReasoningEnvelopes: the tag name, not a plugin, decides.
var (
	reasoningOpen       = regexp.MustCompile(`<\s*([A-Za-z][A-Za-z0-9_:-]{1,40})(?:\s+[^>\n]{0,120})?\s*>`)
	reasoningName       = regexp.MustCompile(`^(?:internal|hidden|private|model|assistant)?(?:thoughts?|thinking|think|reasoning|reason|analysis|scratchpad|scratch|reflection|reflect|deliberation|rationale|chainofthought|cot)$`)
	filterComplete      = regexp.MustCompile(`(?i)<__filter_complete__\s*/?>`)
	manyBreaks          = regexp.MustCompile(`\n{3,}`)
	reasoningClose      = regexp.MustCompile(`(?i)<\s*/\s*([A-Za-z][A-Za-z0-9_:-]{1,40})\s*>`)
	reasoningSeparators = strings.NewReplacer(" ", "", "_", "", "-", "", "\t", "")
)

func isReasoningName(tag string) bool {
	n := strings.ToLower(strings.TrimSpace(tag))
	n = reasoningSeparators.Replace(n)
	return n == "filter" || n == "filtercomplete" || reasoningName.MatchString(n)
}

// WithoutReasoning is the text with its reasoning envelopes removed: a
// closed one with its content, an unclosed one with everything after it.
func WithoutReasoning(text string) string {
	if !strings.Contains(text, "<") {
		return text
	}
	clean := strings.ReplaceAll(text, "\r\n", "\n")
	for {
		cut := false
		for _, m := range reasoningOpen.FindAllStringSubmatchIndex(clean, -1) {
			tag := clean[m[2]:m[3]]
			if !isReasoningName(tag) {
				continue
			}
			end := -1
			for _, c := range reasoningClose.FindAllStringSubmatchIndex(clean[m[1]:], -1) {
				if strings.EqualFold(tag, clean[m[1]+c[2]:m[1]+c[3]]) {
					end = m[1] + c[1]
					break
				}
			}
			if end >= 0 {
				clean = clean[:m[0]] + clean[end:]
			} else {
				clean = clean[:m[0]]
			}
			cut = true
			break
		}
		if !cut {
			break
		}
	}
	clean = filterComplete.ReplaceAllString(clean, "")
	return strings.TrimSpace(manyBreaks.ReplaceAllString(clean, "\n\n"))
}

// Postprocessor final output (AC1 extractPostprocessorCanonicalAssistantText):
// a reply carrying a quality layer's final block is read as that block;
// the legacy compare format is read without its compare block.
var (
	finalTags = []string{"ArchiveCenterFinalOutput", "ArchiveCenterFinal", "ACFinalOutput", "ACFinal",
		"PostprocessorFinalOutput", "PostprocessorFinal", "PostProcessFinal", "FinalAssistantOutput",
		"CanonicalAssistantOutput", "QualityLayerFinalOutput", "QualityLayerFinal"}
	rekoPresent = regexp.MustCompile(`(?i)<\s*ReKo(?:Compare|Result)\b`)
	finalBlocks = func() map[string]*regexp.Regexp {
		out := map[string]*regexp.Regexp{}
		for _, tag := range append(append([]string{}, finalTags...), "ReKoCompare", "ReKoAfter", "ReKoResult") {
			q := regexp.QuoteMeta(tag)
			out[tag] = regexp.MustCompile(`(?is)<\s*` + q + `\b[^>]*>(.*?)<\s*/\s*` + q + `\s*>`)
		}
		return out
	}()
)

// tagged is a whole <tag ...>…</tag> block, its content as the submatch.
func tagged(tag string) *regexp.Regexp {
	return finalBlocks[tag]
}

func blocks(text string, tags ...string) []string {
	var out []string
	for _, tag := range tags {
		for _, m := range tagged(tag).FindAllStringSubmatch(text, -1) {
			if b := strings.TrimSpace(m[1]); b != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

// FinalOutput is the reply as a postprocessor marked it final, else the reply.
func FinalOutput(text string) string {
	if !strings.Contains(text, "<") {
		return text
	}
	if b := blocks(text, finalTags...); len(b) > 0 {
		return strings.Join(b, "\n\n")
	}
	if rekoPresent.MatchString(text) {
		visible := strings.TrimSpace(tagged("ReKoCompare").ReplaceAllString(text, ""))
		if visible == "" {
			visible = strings.Join(blocks(text, "ReKoAfter", "ReKoResult"), "\n\n")
		}
		if visible != "" {
			return visible
		}
	}
	return text
}

// SourceWithoutReasoning is the turn as generation reads it: each reply as
// its final output, without reasoning (AC1 cleaned the assistant text only,
// in this order).
func SourceWithoutReasoning(src Source) Source {
	out := src
	out.Responses = append([]SourceResponse(nil), src.Responses...)
	for i := range out.Responses {
		out.Responses[i].Data = WithoutReasoning(FinalOutput(out.Responses[i].Data))
	}
	return out
}
