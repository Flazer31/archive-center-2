package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Narrative support (1.0 E03, preprocessing step 4, design
// 20261107-ac2-preprocessing/DESIGN-04.md). After the memory is chosen (and
// a preprocessing mode has chosen among it), one AI ("the publisher") may:
//   - write a guide at one of four levels (user 11-08 "4단계로"), each level
//     adding one kind of item so the levels differ plainly: weak =
//     connections (reference), mid = + reactions (may be used), strong = +
//     beats (to follow, placed right before the reply);
//   - improve the user's input for the main model only: clarify the user's
//     own words, or weave related memories in as short phrases (blending was
//     removed and brought back on 2026-11-10 with a head that forbids copying
//     the memory lines). The chat RisuAI keeps is never changed.
//
// Every item must name its basis (a memory line or the dialog); one without
// is dropped from the guide (user 11-08 "빼기") — the guide and the output
// are never held. Secret-lane memories are not shown to the publisher:
// their protection is Go's and a guide or a rewritten input could carry them
// past it. Any failure sends the request as it would go without support.

// narrTimeout bounds one publisher call; narrDeadline is the latest the
// whole memory answer may run from the request's start (the plugin waits
// 100 s for it): the handler's context ends there (review 20 B-03).
const narrTimeout = 60 * time.Second

var narrDeadline = 95 * time.Second // a var only so a test can shorten it

type narrDeps struct {
	s       store.NarrativeSettings
	head    string // the guide's rules: the saved override, "" for the default
	ai      provider.Chatter
	problem string // why the AI is missing ("ai_off", ...)
}

// narrativeFor reads the saved choice and builds the publisher's connection:
// the critic's unless its own is saved. nil when both are off.
func (m *models) narrativeFor() (*narrDeps, string) {
	if m == nil || m.db == nil {
		return nil, ""
	}
	s, err := m.db.NarrativeSettings(m.box)
	if err != nil {
		return nil, "store_failed"
	}
	if s.Level == store.GuideOff && s.Input == store.InputOff {
		return nil, ""
	}
	n := &narrDeps{s: s}
	// a saved publisher prompt replaces the default rules; one that cannot be
	// read leaves the default (the guide still runs)
	if head, err := m.db.PublisherPrompt(); err == nil {
		n.head = head
	}
	cfg := s.AI
	if !s.AIOwn {
		m.mu.RLock()
		saved := m.saved
		m.mu.RUnlock()
		cfg, _ = m.critic(saved)
	}
	cfg.NoRedirect = true // the key and the story go only to the address set
	if cfg.Provider == "" {
		n.problem = "ai_off"
	} else if c, err := provider.New(cfg); err != nil {
		n.problem = err.Error()
	} else {
		n.ai = c
	}
	return n, ""
}

// narrGuide is what goes into the request: AfterMemory right after the
// memory block, BeforeReply as the request's last message (strong only).
type narrGuide struct {
	AfterMemory string `json:"after_memory,omitempty"`
	BeforeReply string `json:"before_reply,omitempty"`
}

// inputRewrite replaces the user's text From (the host row Go read) by To in
// the main model's request only.
type inputRewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

const narrGuideHead = `너는 롤플레이의 다음 응답을 돕는 서사 편집자다. 응답 본문 대신 짧은 가이드를 만든다.

[대화]에는 현재 사용자 입력과 최근 대화가 있다. 끝의 [이번 사용자 입력]은 같은 현재 입력을 다시 보여 준 것이다. 최근 대화는 배경이고, 이번 응답은 [이번 사용자 입력]이 정한 장면에서 이어진다. [기억 줄]은 이번 요청에 함께 넣는 저장된 기억이다.

규칙:
- 이번 사용자 입력의 시간·장소·진행 방향·속도를 따른다. 사용자가 장면을 옮겼다면 이전 장면을 다시 이어 쓰지 않는다.
- 사실은 대화와 기억 줄에서만 가져온다. 새 과거·설정·인물·사건 결과를 만들지 않는다. 추측과 계획은 확정된 사실로 바꾸지 않는다.
- 사용자 캐릭터가 아직 쓰지 않은 말·행동·생각·감정·선택을 정하지 않는다. 사용자가 이미 쓴 행동은 이번 응답의 출발점으로 삼는다.
- 반응과 전개는 앞으로의 응답을 위한 제안이다. 인물이 아는 범위와 관계를 지키며, 아직 일어나지 않은 반응을 과거 사실처럼 쓰지 않는다.
- 이번 응답에 도움이 되는 내용만 쓴다. 기억을 길게 다시 설명하거나 같은 내용을 여러 칸에 반복하지 않는다. 필요 없는 칸은 빈 목록으로 둔다.
- 각 항목의 refs에는 그 내용을 뒷받침하는 기억 줄 번호(예: "L3")나 대화 표식 "D"를 쓴다. "D"는 대화의 실제 내용에 근거한다는 뜻이며, 추측을 사실로 만드는 표식이 아니다. 근거 없는 항목은 쓰지 않는다.
- 사용자 입력과 같은 언어로, 항목마다 한두 문장으로 간결하게 쓴다.`

// narrFields are each level's items, in the order they are explained.
var narrFields = map[string][]string{
	store.GuideWeak:   {"connections"},
	store.GuideMid:    {"connections", "reactions"},
	store.GuideStrong: {"connections", "reactions", "beats"},
}

var narrFieldText = map[string]string{
	"connections": `connections: 이번 장면에서 여전히 유효한 약속·상태·관계와, 그것이 현재 상황에 이어지는 점. 기억 줄 또는 대화에서 찾는다. 단순한 지난 이야기 요약은 쓰지 않는다. 형식 {"text":"…","refs":["L2"]}`,
	"reactions":   `reactions: 이번 입력에 반응할 NPC의 반응 후보. 누구의 어떤 말·행동에 대한 반응인지 드러내고, 가능한 반응으로 쓴다. 사용자 캐릭터의 반응은 쓰지 않는다. 모든 인물을 채울 필요는 없다. 형식 {"who":"인물 이름","text":"…","refs":["D"]}`,
	"beats":       `beats: 이번 응답을 어떤 순서로 이어 갈지 적는 전개 지침. 현재 입력에 대한 반응에서 시작하고, 필요한 만큼 전개한 뒤 이번 응답을 맺는다. part는 "시작", "전개", "마무리" 중 해당하는 것을 쓰며 모든 부분을 채울 필요는 없다. 마무리는 사건 해결이나 다음 날로의 이동을 뜻하지 않는다. 사용자에게 남은 선택을 대신 결정하지 않는다. 형식 {"part":"시작","text":"…","refs":["D"]}`,
}

func narrGuidePrompt(head, level, state, lines, input string) string {
	var b strings.Builder
	if strings.TrimSpace(head) == "" {
		head = narrGuideHead
	}
	b.WriteString(strings.TrimRight(head, "\n") + "\n\n")
	fields := narrFields[level]
	shape := map[string][]any{}
	for _, f := range fields {
		b.WriteString("- " + narrFieldText[f] + "\n")
		shape[f] = []any{}
	}
	b.WriteString("\n")
	out, _ := json.Marshal(shape)
	if lines == "" {
		lines = "(없음)"
	}
	b.WriteString("출력은 JSON 객체 하나만 쓴다. 코드 블록과 해설은 붙이지 않는다. 아래 키만 쓰고 각 값은 목록으로 쓴다. 쓸 내용이 없으면 []로 둔다.\n" + string(out) + "\n\n[대화]\n" + state + "\n\n[기억 줄]\n" + lines + "\n\n[이번 사용자 입력]\n" + input + "\n\n이 입력에서 이어질 이번 응답의 가이드를 지정된 JSON으로 작성한다.")
	return b.String()
}

const narrClarifyHead = `너는 롤플레이 사용자 입력의 문장을 다듬는 편집자다. [대화]는 뜻을 이해하기 위한 문맥이며 현재 입력도 포함한다. 마지막 [사용자 입력]에 있는 글만 다듬는다. 이야기의 다음 부분이나 상대의 답을 쓰지 않는다.

뜻과 말투를 지키면서 문장을 자연스럽고 분명하게 정리한다.
- 문장 나누기, 조사·띄어쓰기·어순, 불필요하게 겹친 표현을 정리할 수 있다.
- 생략된 주어·대상은 현재 입력과 최근 대화에서 누구 또는 무엇을 가리키는지 분명할 때만 보충한다. 여러 뜻으로 읽히는 부분은 임의로 결정하지 말고 그대로 둔다.
- 새 사실·행동·대사·감정·이유·묘사를 더하지 않는다. 대화나 기억의 내용을 설명으로 덧붙이지 않는다.
- 원문의 말투·시점·인칭·언어와 대사·서술·지시의 구분을 지킨다. 따옴표 안 대사와 괄호 안 지시는 그대로 둔다.
- 이름·수치·부정·조건·의문·망설임·강조·행동 순서를 보존한다. 시도나 계획을 완료된 일로 바꾸지 않는다. 짧게 만들기 위해 뜻을 빼지 않는다.
- 이미 분명하고 자연스러우면 바꾸지 않는다. 고칠 부분만 고치고 나머지는 유지한다.

바꾸었다면 출력은 JSON 하나: {"input":"다듬은 사용자 입력 전문"}
바꿀 것이 없으면: {"unchanged":true}
둘 중 하나만 출력하고 코드 블록이나 해설은 붙이지 않는다.`

// narrBlendHead brings memory blending back (user 2026-11-10: "되살리는데
// 이전에는 기억 입력에 있던 내용을 그대로 붙여 넣어서 그래"). The earlier
// head only said to use facts from the memory lines, and the model copied
// the lines; this one says how a memory may enter the input.
const narrBlendHead = `너는 롤플레이 사용자 입력에 지난 일을 녹이는 편집자다. [대화]는 최근 대화이며 현재 입력도 포함한다. [기억 줄]은 이번 요청에 함께 들어가는 저장된 기억이다. 마지막 [사용자 입력]에 있는 글만 고친다. 이야기의 다음 부분이나 상대의 답을 쓰지 않는다.

사용자 입력이 가리키거나 기대고 있는 지난 일이 [기억 줄]에 있을 때만, 그 일을 사용자의 문장 안에 짧은 구절로 녹인다.
- 기억 줄의 문장을 옮겨 적지 않는다. 한 사실은 사용자의 말투와 시점에 맞춘 짧은 구절 하나로 바꿔서, 그 사실과 닿는 문장 안에 넣는다.
- 많아도 세 가지까지만 녹인다. 입력 끝에 설명 문장을 덧붙이거나 목록·머리말·괄호 설명을 만들지 않는다.
- 입력이나 [대화]에 이미 나온 내용은 다시 넣지 않는다. 지금 입력과 닿지 않는 기억은 넣지 않는다. 녹일 것을 찾으려고 애쓰지 않는다.
- [기억 줄]에 있는 사실만 쓴다. 새 사실·행동·대사·감정·이유·묘사를 더하지 않는다.
- 사용자가 맡은 인물이 보고 듣고 아는 것만 녹인다. 다른 인물의 속마음·생각·숨은 사정은 넣지 않는다.
- 녹인 구절 밖의 글은 고치지 않는다. 사용자가 쓴 뜻·행동·행동 순서·말투·시점·인칭·언어를 지키고, 따옴표 안 대사와 괄호 안 지시는 그대로 둔다.

고쳤다면 출력은 JSON 하나: {"input":"고친 사용자 입력 전문","refs":["L2"]}
녹일 것이 없으면: {"unchanged":true}
둘 중 하나만 출력하고 코드 블록이나 해설은 붙이지 않는다.`

// firstJSON decodes the answer's first JSON object into v (a model may add
// prose around it).
func firstJSON(s string, v any) error {
	at := strings.Index(s, "{")
	if at < 0 {
		return errors.New("ai_answer_without_json")
	}
	if err := json.NewDecoder(strings.NewReader(s[at:])).Decode(v); err != nil {
		return errors.New("ai_answer_not_json")
	}
	return nil
}

type narrItem struct {
	Who  string   `json:"who"`
	Part string   `json:"part"`
	Text string   `json:"text"`
	Refs []string `json:"refs"`
}

// grounded says whether an item names a basis that exists: a line shown
// ("L3") or the dialog ("D").
func grounded(it narrItem, shown map[string]bool) bool {
	for _, r := range it.Refs {
		r = strings.TrimSpace(r)
		if r == "D" || shown[r] {
			return true
		}
	}
	return false
}

// renderGuide puts the grounded items in within the ceiling (counted in
// characters, headers included): beats first at strong, then connections,
// then reactions; an item that does not fit is skipped. A ceiling, never a
// fill target.
func renderGuide(level string, items map[string][]narrItem, shown map[string]bool, ceiling int, rec *store.NarrativeRecord) narrGuide {
	type sec struct {
		head  string
		lines []string
	}
	refHead := "[서사 참고 — 지난 일과 인물 상태에 대한 메모다. 연결은 배경으로 참고하고 NPC 반응 후보는 상황에 맞게 골라 쓴다. 모두 본문에 넣을 필요는 없다. 현재 입력의 방향과 속도를 우선한다]"
	if level == store.GuideWeak {
		refHead = "[서사 참고 — 지난 일과 인물 상태에 대한 배경 메모다. 본문에 직접 설명할 필요는 없으며, 현재 입력의 방향과 속도를 우선한다]"
	}
	after := []*sec{{head: "현재 장면의 연결:"}, {head: "NPC 반응 후보:"}}
	before := &sec{}
	render := func() narrGuide {
		var g narrGuide
		var b strings.Builder
		for _, s := range after {
			if len(s.lines) > 0 {
				b.WriteString(s.head + "\n" + strings.Join(s.lines, "\n") + "\n")
			}
		}
		if b.Len() > 0 {
			g.AfterMemory = refHead + "\n" + strings.TrimSuffix(b.String(), "\n")
		}
		if len(before.lines) > 0 {
			g.BeforeReply = "[이번 응답 전개 계획 — 현재 입력의 방향과 속도 안에서 아래 순서에 따라 응답한다. 사용자 캐릭터의 미선택 행동은 대신 정하지 않는다. 아래는 앞으로 쓸 응답의 지침이며, 이미 일어난 사건이 아니다. 계획 문구를 본문에서 해설하지 않는다]\n" + strings.Join(before.lines, "\n")
		}
		return g
	}
	// counted as sent: each part with its title and fingerprint (review 20
	// C-01)
	size := func(g narrGuide) int {
		return utf8.RuneCountInString(store.GuideMessage(g.AfterMemory)) + utf8.RuneCountInString(store.GuideMessage(g.BeforeReply))
	}
	type slot struct {
		field string
		to    *sec
	}
	order := []slot{{"connections", after[0]}, {"reactions", after[1]}}
	if level == store.GuideStrong {
		order = append([]slot{{"beats", before}}, order...)
	}
	allowed := map[string]bool{}
	for _, f := range narrFields[level] {
		allowed[f] = true
	}
	for _, o := range order {
		if !allowed[o.field] {
			continue // a field the level does not have is thrown away
		}
		for _, it := range items[o.field] {
			text := strings.Join(strings.Fields(it.Text), " ")
			if text == "" {
				continue
			}
			if !grounded(it, shown) {
				rec.Dropped++
				continue
			}
			line := "- " + text
			switch o.field {
			case "reactions":
				if w := strings.TrimSpace(it.Who); w != "" {
					line = "- " + w + ": " + text
				}
			case "beats":
				if p := strings.TrimSpace(it.Part); p != "" {
					line = "- " + p + ": " + text
				}
			}
			o.to.lines = append(o.to.lines, line)
			if size(render()) > ceiling {
				o.to.lines = o.to.lines[:len(o.to.lines)-1]
				rec.OverBudget++
				continue
			}
			rec.Kept++
		}
	}
	g := render()
	rec.Chars = size(g)
	return g
}

// narrLines are the delivered non-secret records' lines, labelled L1.. in
// the delivery's order.
func narrLines(d store.Delivery) (string, map[string]bool) {
	var b strings.Builder
	shown := map[string]bool{}
	n := 0
	for i, id := range d.IDs {
		if d.SecretIDs[id] || i >= len(d.Lines) {
			continue
		}
		n++
		label := fmt.Sprintf("L%d", n)
		shown[label] = true
		fmt.Fprintf(&b, "%s %s\n", label, strings.TrimPrefix(d.Lines[i], "- "))
	}
	return b.String(), shown
}

// narrative runs the guide and the input improvement (at once) over the
// delivery being sent. started is the request's start.
func narrative(ctx context.Context, n *narrDeps, d store.Delivery, scope []memory.ScopedTurn, input string, started time.Time) (narrGuide, *inputRewrite, *store.NarrativeRecord) {
	t0 := time.Now()
	rec := &store.NarrativeRecord{Level: n.s.Level, Input: n.s.Input, CharsMax: n.s.Chars}
	defer func() { rec.MS = time.Since(t0).Milliseconds() }()
	var g narrGuide
	var rw *inputRewrite
	lines, shown := narrLines(d)
	state := prepState(input, scope, d.Window)
	wantGuide := n.s.Level != store.GuideOff
	wantInput := n.s.Input != store.InputOff
	switch {
	case wantGuide && n.ai == nil:
		rec.Guide = n.problem
		wantGuide = false
	case wantGuide && n.s.Chars == 0:
		rec.Guide = "no_budget"
		wantGuide = false
	}
	switch {
	case wantInput && n.ai == nil:
		rec.InputResult = n.problem
		wantInput = false
	case wantInput && strings.TrimSpace(input) == "":
		rec.InputResult = "no_input_text"
		wantInput = false
	case wantInput && n.s.Input == store.InputBlend && len(shown) == 0:
		// nothing delivered to blend: the input goes as the user wrote it
		rec.InputResult = "unchanged"
		wantInput = false
	}
	if !wantGuide && !wantInput {
		return g, nil, rec
	}
	limit := narrTimeout
	if left := narrDeadline - time.Since(started); left < limit {
		limit = left
	}
	if limit <= 0 {
		if wantGuide {
			rec.Guide = "no_time_left"
		}
		if wantInput {
			rec.InputResult = "no_time_left"
		}
		return g, nil, rec
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	if wantGuide {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := n.ai.Chat(ctx, narrGuidePrompt(n.head, n.s.Level, state, lines, input))
			mu.Lock()
			defer mu.Unlock()
			rec.AICalls++
			if err == nil {
				items := map[string][]narrItem{}
				if err = firstJSON(r.Content, &items); err == nil {
					g = renderGuide(n.s.Level, items, shown, n.s.Chars, rec)
					if g.AfterMemory == "" && g.BeforeReply == "" {
						rec.Guide = "nothing_kept"
					} else {
						rec.Guide = "made"
						rec.GuideText = strings.TrimSpace(g.AfterMemory + "\n\n" + g.BeforeReply)
					}
					return
				}
			}
			rec.Guide, rec.GuideError = "ai_failed", err.Error()
		}()
	}
	if wantInput {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prompt := narrClarifyHead + "\n\n[대화]\n" + state + "\n\n[사용자 입력]\n" + input
			if n.s.Input == store.InputBlend {
				prompt = narrBlendHead + "\n\n[대화]\n" + state + "\n\n[기억 줄]\n" + lines + "\n[사용자 입력]\n" + input
			}
			r, err := n.ai.Chat(ctx, prompt)
			mu.Lock()
			defer mu.Unlock()
			rec.AICalls++
			if err == nil {
				var a struct {
					Input     string `json:"input"`
					Unchanged bool   `json:"unchanged"`
				}
				if err = firstJSON(r.Content, &a); err == nil {
					to := strings.TrimSpace(a.Input)
					if a.Unchanged || to == "" || to == strings.TrimSpace(input) {
						rec.InputResult = "unchanged"
						return
					}
					rw = &inputRewrite{From: input, To: to}
					rec.InputResult, rec.InputBefore, rec.InputAfter = "changed", input, to
					return
				}
			}
			rec.InputResult, rec.InputError = "ai_failed", err.Error()
		}()
	}
	wg.Wait()
	return g, rw, rec
}
