package memory

import "testing"

// PlainText: HTML element tags out with their words kept, a break where a
// block begins or ends, a space between table cells, quoted ">" inside
// attributes read as part of the tag, style / script / comment blocks
// gone, basic entities written out; angle brackets that are no HTML
// element and fenced code stay, and a text with no HTML element is
// unchanged (its entities and blank lines too).
func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"평범한 글 그대로":                                     "평범한 글 그대로",
		"A & B\n\n\n\nC &amp; D":                        "A & B\n\n\n\nC &amp; D",
		"<b>안</b>녕":                                     "안녕",
		"첫 줄<br>둘째 줄":                                   "첫 줄\n둘째 줄",
		"앞<div>뒤</div>":                                 "앞\n뒤\n",
		"<table><tr><td>5</td><td>10</td></tr></table>": "\n\n 5  10 \n\n", // cells stay apart
		`<span title="설명 > 비공개">본문</span>`:              "본문",
		"<style>.a{color:red}</style>본문":                "본문",
		"앞<!-- 숨은 주석 -->뒤":                              "앞뒤",
		"사랑해 <3 정말":                                     "사랑해 <3 정말",
		"<시스템> 알림":                                      "<시스템> 알림",
		"<OOC> 다음엔 쉬자 </OOC>":                           "<OOC> 다음엔 쉬자 </OOC>",
		"<user>가 말했다. 이름은 <Alice>":                      "<user>가 말했다. 이름은 <Alice>",
		"<i>A</i> &amp; B &lt;태그&gt;":                   "A & B <태그>",
		"<span style=\"x\">취기 18</span>":                "취기 18",
		"글 ```<script>x()</script>``` <b>끝</b>":         "글 ```<script>x()</script>``` 끝",
	} {
		if got := PlainText(in); got != want {
			t.Errorf("PlainText(%q) = %q, want %q", in, got, want)
		}
	}
	src := Source{Members: []SourceMember{{Data: "<i>입력</i>"}}, Responses: []SourceResponse{{Data: "<b>답</b>"}}}
	got := SourceWithoutMarkup(src)
	if got.Members[0].Data != "입력" || got.Responses[0].Data != "답" || src.Members[0].Data != "<i>입력</i>" || src.Responses[0].Data != "<b>답</b>" {
		t.Fatalf("source %+v (the given one must stay %+v)", got, src)
	}
}
