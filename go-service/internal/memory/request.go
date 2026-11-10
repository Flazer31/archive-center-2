package memory

import (
	"fmt"
	"sort"
	"strings"
)

// Instructions are the generation instructions (contract v2 §4 order). They
// are written for light models: short steps, one JSON object at the end.
const Instructions = `You record the memory of a roleplay chat, one turn at a time.
Read THIS TURN and write only what is new or changed in it. Write body and names in the language of the REPLY lines (the story's language), even when USER lines use another; q is an exact quote and keeps its own language.

Steps:
1. Find what is new or changed in this turn. If it is already in KNOWN, do not write it again:
   - same thing mentioned again: {"ref": "<KNOWN id>", "q": "<quote>"}
   - something known that changed: write the new record with "was": "<KNOWN id>" (or a list of ids)
2. For each new thing choose one kind: "event" (what happened), "fact" (what stays true), "view" (what one person thinks or feels).
3. Do not leave out conditions, negation, who was involved, when, who told whom, and who knows or must not know.
   Look for what the text shows someone hides, does not know or is wrong about (a hidden thing or origin, a false identity, a lie or excuse told, a mistaken belief) and the reasons the text gives.
4. Check the lists: fact mode is one of 상태, 성향, 말투, 습관, 관계, 경계, 세계 규칙, 의무, 질문·위험, 공동 목표.
   view aspect is one of belief, memory, interpretation, emotion, motive, goal, intent.
5. Think briefly. Write the JSON once, at the end.

Rules:
- elapsed_days: at the top level, write the number of whole story days that passed since the previous turn (0, 1, 2, ...). Count only passage to the current scene, not a recalled or planned event. On the first turn, or when no passage is clear, write 0. This is a turn-level delta, not the chat's cumulative day number.
- Use the ids shown in KNOWN exactly. For new items use short temporary ids: n1, n2 for names, m1, m2 for memories, s1 for scenes, l1 for links.
- Never invent a value (emotion, goal, number, weather, date) that the text does not give. Leave fields out when the text has nothing for them.
- time: a STORY DATE, when given, is the story date after this turn; use it for what happens now. Otherwise write the date or time the text gives. If the text only says how much time passed ("the next day", "three days later"), count days from the first day of the chat: "day 0", "day 1", ...; if the earlier days are not known, write the words the text uses. A recalled past event keeps its own time, not the current date.
- q is a short exact quote from THIS TURN that supports the item.
- A state first given in THIS TURN and not already in KNOWN is new, including values in status lines (what someone has or carries, money, where they are). Record it once: as a fact for an initial state, or in the event's effects for a change. Do not repeat the same changed state as another fact.
- Changed values: in an event's "effects" write {"about", "slot", "before", "value", "delta", "was"}; for numbers give the old value, the new value and the change.
- "at" is the id of the event a fact or view comes from: a KNOWN event (M12) or a new event in this answer (m1). Dates and times go in an event's "time". Leave "at" out when there is no such event.
- New items and refs go inside the "memories" array. actor, target, object, place, speaker and listener are arrays inside "roles". "affect" is an object {"intensity": "...", "persistence": "순간|남음|누적 태도"}. Write only what the text says.
- Promises, requests, plans and limits are recorded in the turn they are said, keeping who said what to whom, the condition and the time:
  - 의무 (fact): what someone has to do because they promised it, accepted a request, or because the text shows a rule or role that binds them. about = who owes it, to = to whom. How it is said (an order, a request) does not decide it; the text does.
  - A request or demand the other has not accepted: a view of the asker (aspect goal), not an obligation of the one asked.
  - 경계 (fact): a limit someone sets on their own body, things or privacy, or a rule on what others may do; it holds without the other's agreement. A request to come or to do something is not a limit.
  - One person's own plan or decision: a view of that person (aspect goal or intent). A plan several people agree on: a fact (mode 공동 목표).
- An obligation done or broken: point to it ("effects": [{"about": "<obligation id>", "status": "fulfilled"}] or a new fact with "was"). Do it once. A repeating obligation stays open after one round.
- Names: one entity per new name. "A의 B", "A에 있는 B", a building in a place, or a grade of a thing is not the same identity. Write location and kind as facts. Do not put quantities or states in a name.
- Scenes: KNOWN shows the current scene. If this turn goes on in it or ends it, write {"ref": "<current scene id>", "change": "continue"} or "close". Write {"id": "s1", "change": "open"} only when a new scene starts.
- same_as only when the text says two names are the same one (alias, disguise, true name).
- knows / unaware / hide_from / allow_to only when the text shows who knows, who does not know, or who must not be told.
- A secret the text shows: write the true thing once, on one item, with unaware or hide_from naming only the people the text shows not knowing it or kept from it. Do not copy it into another item.
- Something false that someone says (a lie, an excuse, a cover story): an event with speaker and listener, the body saying what was said, and "differs": "yes". A belief or view the text shows to be wrong: "differs": "yes".
- Reasons: when the text says why someone believes, feels or decides something, write that reason in the view's body.
- fact status: active, fulfilled, partial, missed, paused, resumed, cancelled, impossible, refused, changed (질문·위험: active, resolved, changed). expressed: 속으로만, 행동으로, 고백, 억누름.
- A personal goal is a view (aspect goal); a goal several people agreed on is a fact (mode 공동 목표).
- Every view has an owner: the one person whose thought, belief or feeling it is (not the person it is about).
- about, owner, to and each role entry hold ONE id or name. For several, write several items or list them in roles; a feeling or change the text gives to several people is one view for each of them.

JSON shape (all arrays optional, fields optional unless needed):
{"elapsed_days":0,"entities":[{"id":"n1","name":"","type":"person|place|thing|group","q":"","same_as":"","how":"alias|disguise|true_name"}],
 "memories":[
  {"id":"m1","kind":"event","body":"","q":"","imp":0-10,"roles":{"actor":[],"target":[],"object":[],"place":[],"speaker":[],"listener":[]},"time":"","effects":[{"about":"","slot":"","before":"","value":"","delta":"","was":"","status":""}],"outcome":"attempt|success|partial|failed|interrupted|cancelled","setting":"","seen_by":[],"differs":"yes|no|unknown"},
  {"id":"m2","kind":"fact","about":"","slot":"","mode":"","body":"","q":"","to":"","scope":"","terms":"","status":"","at":"","was":""},
  {"id":"m3","kind":"view","owner":"","about":"","aspect":"","body":"","q":"","basis":{"via":"경험|전언|추론|소문|기억|내면 서술","from":"","ref":"","confidence":""},"affect":{"intensity":"","persistence":"순간|남음|누적 태도"},"expressed":"","differs":"yes|no|unknown"},
  {"ref":"M12","q":""}],
 "scenes":[{"id":"s1","change":"open|continue|close","q":""}],
 "links":[{"from":"","rel":"causes|condition_of|changes|fulfills|recalls|contradicts|same_fact","to":"","q":""}]}
Inside any item (not beside the arrays) you may also write "knows":[{"who":"","via":"직접|전언|추측","from":"","extent":"전부|부분","part":"","ref":"","q":""}], "unaware":[], "hide_from":[], "allow_to":[].`

// RenderKnown writes the selection for the model, each line with its id.
// It keeps what tells records apart (when, who, to whom, in what scope, on
// what basis, with what result) and marks records a later one replaced.
// Quotes of memories are left out; a name shows the quote Select kept (the
// one that tells two same-named entities apart).
func RenderKnown(sel Selection, superseded map[string]bool) string {
	var b strings.Builder
	field := func(name, v string) {
		if v != "" {
			fmt.Fprintf(&b, " | %s %s", name, v)
		}
	}
	list := func(name string, l []string) {
		if len(l) > 0 {
			field(name, strings.Join(l, ","))
		}
	}
	for _, n := range sel.Names {
		fmt.Fprintf(&b, "[%s] %s", n.ID, n.Name)
		if n.Type != "" {
			fmt.Fprintf(&b, " (%s)", n.Type)
		}
		fmt.Fprintf(&b, " T%d", n.Turn)
		if n.Q != "" {
			fmt.Fprintf(&b, " %q", n.Q)
		}
		b.WriteString("\n")
	}
	for _, r := range sel.Relations {
		fmt.Fprintf(&b, "[%s] %s same_as %s (%s) T%d", r.ID, r.Name, r.SameAs, r.How, r.Turn)
		if r.Q != "" {
			fmt.Fprintf(&b, " %q", r.Q)
		}
		b.WriteString("\n")
	}
	for _, m := range sel.Memories {
		fmt.Fprintf(&b, "[%s] T%d", m.ID, m.Turn)
		if m.Date != "" {
			fmt.Fprintf(&b, " (%s)", m.Date)
		}
		fmt.Fprintf(&b, " %s", m.Kind)
		if m.Mode != "" {
			fmt.Fprintf(&b, "/%s", m.Mode)
		}
		if m.Aspect != "" {
			fmt.Fprintf(&b, "/%s", m.Aspect)
		}
		if m.Owner != "" {
			fmt.Fprintf(&b, " owner %s", m.Owner)
		}
		if m.About != "" {
			fmt.Fprintf(&b, " about %s", m.About)
		}
		if m.Slot != "" {
			fmt.Fprintf(&b, " slot %q", m.Slot)
		}
		if len(m.To) > 0 {
			fmt.Fprintf(&b, " to %s", strings.Join(m.To, ","))
		}
		fmt.Fprintf(&b, ": %s", m.Body)
		field("time", m.Time)
		field("outcome", m.Outcome)
		if r := m.Roles; r != nil {
			list("actor", r.Actor)
			list("target", r.Target)
			list("object", r.Object)
			list("place", r.Place)
			list("speaker", r.Speaker)
			list("listener", r.Listener)
		}
		field("setting", m.Setting)
		list("seen_by", m.SeenBy)
		field("scope", m.Scope)
		field("terms", m.Terms)
		field("status", m.Status)
		if bs := m.Basis; bs != nil {
			field("basis", strings.TrimSpace(strings.Join([]string{bs.Via, prefixed("from ", bs.From), prefixed("ref ", bs.Ref), bs.Confidence}, " ")))
		}
		if a := m.Affect; a != nil {
			field("affect", strings.TrimSpace(a.Intensity+" "+a.Persistence))
		}
		field("expressed", m.Expressed)
		field("differs", m.Differs)
		field("scene", m.Scene)
		field("at", m.At)
		list("was", m.Was)
		if superseded[m.ID] {
			b.WriteString(" | earlier value")
		}
		if len(m.Changed) > 0 {
			b.WriteString(" | changed later: " + strings.Join(m.Changed, ", "))
		}
		b.WriteString("\n")
		for k, e := range m.Effects {
			writeEffect(&b, fmt.Sprintf("%s.%d", m.ID, k+1), 0, e, "", superseded)
		}
	}
	for _, e := range sel.Effects {
		writeEffect(&b, e.Ref, e.Turn, e.Effect, e.Subject, superseded)
	}
	for _, l := range sel.Links {
		fmt.Fprintf(&b, "[%s] T%d %s %s %s\n", l.ID, l.Turn, l.From, l.Rel, l.To)
	}
	for _, a := range sel.Awareness {
		fmt.Fprintf(&b, "  {%s} %s: %s", a.Target, a.Kind, a.Who)
		if a.Via != "" {
			fmt.Fprintf(&b, " via %s", a.Via)
		}
		if a.From != "" {
			fmt.Fprintf(&b, " from %s", a.From)
		}
		if a.Extent != "" {
			fmt.Fprintf(&b, " extent %s", a.Extent)
		}
		if a.Part != "" {
			fmt.Fprintf(&b, " part %q", a.Part)
		}
		if a.Ref != "" {
			fmt.Fprintf(&b, " ref %s", a.Ref)
		}
		b.WriteString("\n")
	}
	if sel.Scene != "" {
		fmt.Fprintf(&b, "current scene: %s\n", sel.Scene)
	}
	return b.String()
}

// writeEffect writes one effect line; turn is given when the effect is
// shown without its event.
func writeEffect(b *strings.Builder, ref string, turn int, e Effect, subject string, superseded map[string]bool) {
	fmt.Fprintf(b, "  [%s]", ref)
	if turn > 0 {
		fmt.Fprintf(b, " T%d", turn)
	}
	fmt.Fprintf(b, " effect %s", e.About)
	if subject != "" {
		fmt.Fprintf(b, " (of %s)", subject)
	}
	if e.Slot != "" {
		fmt.Fprintf(b, " %q", e.Slot)
	}
	if e.Before != "" || e.Value != "" {
		fmt.Fprintf(b, ": %s -> %s", e.Before, e.Value)
	}
	if e.Delta != "" {
		fmt.Fprintf(b, " (%s)", e.Delta)
	}
	if e.Status != "" {
		fmt.Fprintf(b, " status %s", e.Status)
	}
	if len(e.Was) > 0 {
		fmt.Fprintf(b, " | was %s", strings.Join(e.Was, ","))
	}
	if superseded[ref] {
		b.WriteString(" | earlier value")
	}
	b.WriteString("\n")
}

func prefixed(p, v string) string {
	if v == "" {
		return ""
	}
	return p + v
}

// TurnText is the turn's text as the model reads it: the input bundle in
// order, then the reply parts in order (originals, not translations).
func TurnText(src Source) string {
	var b strings.Builder
	for _, m := range src.Members {
		b.WriteString("USER: " + m.Data + "\n")
	}
	for _, r := range src.Responses {
		b.WriteString("REPLY: " + r.Data + "\n")
	}
	return b.String()
}

// Prompt is the full model input for one turn.
func Prompt(src Source, known string) string {
	return PromptWithInstructions(src, known, Instructions)
}

// PromptWithInstructions replaces only the editable prefix, retaining the
// source/KNOWN formatting and story date owned by the backend.
func PromptWithInstructions(src Source, known, instructions string) string {
	return PromptWithText(src, known, instructions, TurnText(src))
}

// PromptWithText reuses the exact turn text already read by Select.
func PromptWithText(src Source, known, instructions, text string) string {
	if strings.TrimSpace(known) == "" {
		known = "(nothing yet)\n"
	}
	date := ""
	if t := DateText(src.Dates); t != "" {
		date = "\nSTORY DATE (chat variables after this turn): " + t + "\n"
	}
	return instructions + "\n\nKNOWN:\n" + known + date + "\nTHIS TURN (T" + fmt.Sprint(src.Turn) + "):\n" + text
}

// DateText writes date variables as "name = value; ..." in name order.
func DateText(dates map[string]string) string {
	names := make([]string, 0, len(dates))
	for k := range dates {
		names = append(names, k)
	}
	sort.Strings(names)
	var parts []string
	for _, k := range names {
		parts = append(parts, k+" = "+dates[k])
	}
	return strings.Join(parts, "; ")
}

// BodyRules is added to the critic's instructions only while body tracking
// is on (1.0 E09/E10, AC1's body_events rules): the model records body facts
// the text states and never calculates or guesses them.
const BodyRules = `
Body facts (only what THIS TURN's text states; write nothing otherwise):
At the top level you may add "body_events":[{"kind":"","who":"","when":"","scope":"","visibility":"","partners":[],"exposure":{"classification":"","partner_compatibility":"","contraception":""},"paternity":{"status":"","candidates":[]},"q":""}].
- kind is one of period_start, pregnancy_confirmed, pregnancy_ended, recovery_started, recovery_ended, conception_exposure. who is the person's name id (an id from KNOWN or this answer's entities).
- period_start: a period starts in the text. pregnancy_confirmed: the text establishes a pregnancy. pregnancy_ended: birth, loss or end of a pregnancy. recovery_started / recovery_ended: recovery after it.
- conception_exposure: an act that could cause pregnancy happens in the text. partners are the other people's name ids. exposure.classification is potentially_conceiving, not_potentially_conceiving or unknown; partner_compatibility is compatible, incompatible or unknown; contraception is none, present or unknown — write unknown when the text does not say.
- pregnancy_confirmed may give paternity: status confirmed only when the story establishes the parent (not marriage, suspicion or closeness); candidates otherwise; unknown when none is named.
- when: the time the text gives for it; leave it empty when it happens in this turn. scope: flashback, planned or hypothetical when it is not happening now; leave it empty otherwise.
- visibility: public when other people in the scene openly know it, else private. A body fact never means anyone knows it.
- Never write probabilities, cycle days or due dates. Do not infer a pregnancy or a period from a delay, desire, mood or repetition. Do not invent symptoms or a birth.
- q is a short exact quote from THIS TURN.`
