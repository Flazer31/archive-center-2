package memory

import (
	"regexp"
	"strings"
)

// The critic reads a turn without the HTML a card or another plugin puts in
// the chat (an illustration plugin wraps a reply's prose in HTML; 16.7% of
// the critic's turn text was tags, the prose itself unchanged). Only HTML
// elements are taken out, the words inside them stay; style, script and
// comment blocks go whole. Angle brackets that are no HTML element
// ("<OOC>", "<user>", "<Alice>", "<3", "<시스템>") and fenced code (```) stay
// as written.
const htmlElements = `a|abbr|article|aside|b|big|blockquote|body|br|caption|center|code|col|colgroup|dd|del|details|dfn|div|dl|dt|em|figcaption|figure|font|footer|h[1-6]|head|header|hr|html|i|img|ins|kbd|label|li|main|mark|nav|ol|p|picture|pre|q|rp|rt|ruby|s|samp|section|small|source|span|strike|strong|sub|summary|sup|svg|table|tbody|td|tfoot|th|thead|tr|tt|u|ul|var|wbr|path|g|circle|rect|polygon|line|defs|use|button|input|textarea|select|option|form|iframe|video|audio|canvas`

var (
	markupBlock = regexp.MustCompile(`(?is)<(?:style|script)\b(?:[^>"']|"[^"]*"|'[^']*')*>.*?</(?:style|script)\s*>|<!--.*?-->`)
	// an element tag: its name, then attributes whose quoted values may hold ">"
	markupTag  = regexp.MustCompile(`(?i)<(/?)(` + htmlElements + `)\b(?:[^>"']|"[^"]*"|'[^']*')*?/?>`)
	blankLines = regexp.MustCompile(`\n[ \t]*(?:\n[ \t]*){2,}`)
	entity     = strings.NewReplacer("&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&amp;", "&")
	// tags that begin or end a block or line, and table cells
	breakTags = map[string]bool{"br": true, "p": true, "div": true, "li": true, "tr": true, "blockquote": true, "section": true, "article": true, "header": true, "footer": true, "hr": true, "table": true, "ul": true, "ol": true, "dl": true, "dt": true, "dd": true, "details": true, "summary": true, "figure": true, "figcaption": true, "main": true, "nav": true, "aside": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true}
	cellTags  = map[string]bool{"td": true, "th": true}
)

// PlainText is a text without its HTML: element tags out (a line break
// where a block begins or ends, a space between table cells), the words
// inside kept, basic entities written out. A text with no HTML element
// comes back exactly as it was; fenced code is kept as written.
func PlainText(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	parts := strings.Split(s, "```")
	changed := false
	for i := 0; i < len(parts); i += 2 { // even parts are outside fenced code
		p := markupBlock.ReplaceAllString(parts[i], "")
		p = markupTag.ReplaceAllStringFunc(p, func(tag string) string {
			name := strings.ToLower(markupTag.FindStringSubmatch(tag)[2])
			switch {
			case breakTags[name]:
				return "\n"
			case cellTags[name]:
				return " "
			}
			return ""
		})
		if p != parts[i] {
			parts[i] = blankLines.ReplaceAllString(entity.Replace(p), "\n\n")
			changed = true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(parts, "```")
}

// SourceWithoutMarkup is the source as the critic reads it: its inputs and
// replies as PlainText. The stored source keeps the text as it came.
func SourceWithoutMarkup(src Source) Source {
	out := src
	out.Members = append([]SourceMember(nil), src.Members...)
	for i := range out.Members {
		out.Members[i].Data = PlainText(out.Members[i].Data)
	}
	out.Responses = append([]SourceResponse(nil), src.Responses...)
	for i := range out.Responses {
		out.Responses[i].Data = PlainText(out.Responses[i].Data)
	}
	return out
}
