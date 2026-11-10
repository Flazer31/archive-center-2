package memory

import "testing"

// The reasoning envelopes AC1 removed; other tags and the story stay.
func TestWithoutReasoning(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"<Thoughts>\n- 계획: 비밀을 밝힌다\n</Thoughts>\n# 응답\n본문", "# 응답\n본문"},
		{"앞 <thinking mode=\"x\">생각</thinking> 뒤", "앞  뒤"},
		{"본문 <reasoning>끝나지 않은 추론", "본문"},
		{"<Hidden_Thoughts>a</hidden_thoughts>본문", "본문"},
		{"<lb-xnai id=\"s\">{{inlay::x}}</lb-xnai> <div class=\"a\">그림</div> 본문", "<lb-xnai id=\"s\">{{inlay::x}}</lb-xnai> <div class=\"a\">그림</div> 본문"},
		{"<Thoughts>a</Thoughts>가<think>b</think>나<__filter_complete__/>", "가나"},
		{"생각 없는 본문", "생각 없는 본문"},
	} {
		if got := WithoutReasoning(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

// A postprocessor's final block is what generation reads (AC1).
func TestFinalOutput(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"초안 <ACFinal>최종 응답</ACFinal>", "최종 응답"},
		// blocks are collected in the tag list order, as AC1 does
		{"<QualityLayerFinalOutput lang=\"ko\">가</QualityLayerFinalOutput>x<acfinal>나</acfinal>", "나\n\n가"},
		{"보이는 본문<ReKoCompare>비교 초안</ReKoCompare>", "보이는 본문"},
		{"<ReKoCompare>비교</ReKoCompare><ReKoResult>결과</ReKoResult>", "<ReKoResult>결과</ReKoResult>"},
		{"평범한 응답 <b>굵게</b>", "평범한 응답 <b>굵게</b>"},
	} {
		if got := FinalOutput(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
	src := Source{Responses: []SourceResponse{{Data: "<Thoughts>계획</Thoughts>초안<ACFinal>최종</ACFinal>"}}}
	if got := SourceWithoutReasoning(src).Responses[0].Data; got != "최종" {
		t.Fatalf("generation read %q", got)
	}
}
