package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
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
   - A new record with "was" replaces that KNOWN record whole: the KNOWN record is no longer shown. When only part of it changed, write in the new record every detail of the KNOWN record that is still true (names, aliases, conditions, steps, who and where), and the change.
2. For each new thing choose one kind: "event" (what happened), "fact" (what stays true), "view" (what one person thinks or feels).
3. Do not leave out conditions, negation, who was involved, when, who told whom, and who knows or must not know.
   Look for what the text shows someone hides, does not know or is wrong about (a hidden thing or origin, a false identity, a lie or excuse told, a mistaken belief) and the reasons the text gives.
   When the text explains a plan, item, ability or rule, keep its stated conditions, limits, exceptions and what they allow in the record that describes it, even before anyone uses it. For a proposal, keep who proposed it.
   In the record of an instruction, keep where or when it applies and what it expressly requires or forbids. If the speaker forbids one choice and gives another, name both.
4. Check the lists: fact mode is one of 정체, 상태, 성향, 말투, 습관, 관계, 경계, 세계 규칙, 의무, 질문·위험, 공동 목표.
   view aspect is one of belief, memory, interpretation, emotion, motive, goal, intent.
5. Think briefly. Write the JSON once, at the end.

Rules:
- elapsed_days: at the top level, write the number of whole story days that passed since the previous turn (0, 1, 2, ...). Count only passage to the current scene, not a recalled or planned event. On the first turn, or when no passage is clear, write 0. This is a turn-level delta, not the chat's cumulative day number.
- story_date: at the top level, when THIS TURN's text states the story's calendar date for now (year, month and day, in any form or language), write it as yyyy/mm/dd. Leave it empty when the text gives no such date or only a part of it; never compute or guess it. When a STORY DATE is given, leave story_date empty.
- Use the ids shown in KNOWN exactly. For new items use short temporary ids: n1, n2 for names, m1, m2 for memories, s1 for scenes, l1 for links.
- Never invent a value (emotion, goal, number, weather, date) that the text does not give. Leave fields out when the text has nothing for them.
- time: a STORY DATE, when given, is the story date after this turn; use it for what happens now. Otherwise write the date or time the text gives. If the text only says how much time passed ("the next day", "three days later"), count days from the first day of the chat: "day 0", "day 1", ...; if the earlier days are not known, write the words the text uses. A recalled past event keeps its own time, not the current date.
- q is a short exact quote from THIS TURN that supports the item.
- A state first given in THIS TURN and not already in KNOWN is new, including values in status lines (what someone has or carries, money, where they are). Record it once: as a fact for an initial state, or in the event's effects for a change. Do not repeat the same changed state as another fact.
- Changed values: in an event's "effects" write {"about", "slot", "before", "value", "delta", "was"}; for numbers give the old value, the new value and the change.
- "at" is the id of the event a fact or view comes from: a KNOWN event (M12) or a new event in this answer (m1). Dates and times go in an event's "time". Leave "at" out when there is no such event.
- New items and refs go inside the "memories" array. actor, target, object, place, speaker and listener are arrays inside "roles".
- For a sale, gift, loan or return, follow the item that changes hands. Put that item in object, only those handing it over in actor, and only those receiving it in target, even when the buyer is the subject of the sentence. Say which kind of transfer it was in the body, and put money paid in the event's effects. On a loan the borrower holds the item temporarily and ownership stays as it was.
  - If an item comes back for a refund, its return sets the direction of those roles, and the money paid back goes in effects. A refund by itself does not establish that the item came back.
  - For a reservation awaiting collection, name the reserved item in object and leave its target empty. A deposit paid now belongs in effects. If a different item was handed over, say who received that item in the body. Keep any agreed delivery terms (item, price and when it is due) with the commitment under the promise rules below.
- "affect" is an object {"intensity": "...", "persistence": "순간|남음|누적 태도"}. Write only what the text says.
- Promises, requests, plans and limits are recorded in the turn they are said, keeping who said what to whom, the condition and the time:
  - 의무 (fact): what someone has to do because they promised it, accepted a request, or because the text shows a rule or role that binds them. about = who owes it, to = to whom. How it is said (an order, a request) does not decide it; the text does.
  - A request or demand the other has not accepted: a view of the asker (aspect goal), not an obligation of the one asked.
  - 경계 (fact): a limit someone sets on their own body, things or privacy, or a rule on what others may do; it holds without the other's agreement. A request to come or to do something is not a limit.
  - One person's own plan or decision: a view of that person (aspect goal or intent). A plan several people agree on: a fact (mode 공동 목표).
- An obligation done or broken: write it in "closed" (below). Do it once. A repeating obligation stays open after one round.
- Names: one entity per new name. "A의 B", "A에 있는 B", a building in a place, or a grade of a thing is not the same identity. Write location and kind as facts. Do not put quantities or states in a name.
  - When the text first identifies a person or group, use its description to write who they are as 정체 (their stated role, affiliation or lasting identifying features). What someone is wearing or holding just now follows the state and event rules.
- Scenes: KNOWN shows the current scene. If this turn goes on in it or ends it, write {"ref": "<current scene id>", "change": "continue"} or "close". Write {"id": "s1", "change": "open"} only when a new scene starts.
- same_as only when the text says two names are the same one (alias, disguise, true name).
- When it matters who can recognize someone, say in the encounter who saw whom and which name or title the text shows each person heard or used for them. Seeing a face or hearing a cover name does not by itself reveal the identity behind it.
- For a secret, who knows, who does not know and who must not be told go in "boundaries" (below), naming only the people the text shows.
- A secret the text shows: write the true thing once, on one item; who does not know it or is kept from it goes in "boundaries". Do not copy it into another item.
- Something false that someone says (a lie, an excuse, a cover story): an event with speaker and listener, the body saying what was said, and "differs": "yes". A belief or view the text shows to be wrong: "differs": "yes".
- Reasons: when the text says why someone believes, feels or decides something, write that reason in the view's body.
- fact status: active, fulfilled, partial, missed, paused, resumed, cancelled, impossible, refused, changed (질문·위험: active, resolved, changed). expressed: 속으로만, 행동으로, 고백, 억누름.
- A personal goal is a view (aspect goal); a goal several people agreed on is a fact (mode 공동 목표).
- Every view has an owner: the one person whose thought, belief or feeling it is (not the person it is about).
- about, owner, to and each role entry hold ONE id or name. For several, write several items or list them in roles; a feeling or change the text gives to several people is one view for each of them.

JSON shape (all arrays optional, fields optional unless needed):
{"elapsed_days":0,"story_date":"","entities":[{"id":"n1","name":"","type":"person|place|thing|group","q":"","same_as":"","how":"alias|disguise|true_name"}],
 "memories":[
  {"id":"m1","kind":"event","body":"","q":"","imp":0-10,"roles":{"actor":[],"target":[],"object":[],"place":[],"speaker":[],"listener":[]},"time":"","effects":[{"about":"","slot":"","before":"","value":"","delta":"","was":"","status":""}],"outcome":"attempt|success|partial|failed|interrupted|cancelled","setting":"","seen_by":[],"differs":"yes|no|unknown"},
  {"id":"m2","kind":"fact","about":"","slot":"","mode":"","body":"","q":"","to":"","scope":"","terms":"","status":"","at":"","was":""},
  {"id":"m3","kind":"view","owner":"","about":"","aspect":"","body":"","q":"","basis":{"via":"경험|전언|추론|소문|기억|내면 서술","from":"","ref":"","confidence":""},"affect":{"intensity":"","persistence":"순간|남음|누적 태도"},"expressed":"","differs":"yes|no|unknown","unlike":""},
  {"ref":"M12","q":""}],
 "scenes":[{"id":"s1","change":"open|continue|close","q":""}],
 "links":[{"from":"","rel":"causes|condition_of|changes|fulfills|recalls|contradicts|same_fact","to":"","q":""}]}
Inside any item (not beside the arrays) you may also write how someone came to know it, "knows":[{"who":"","via":"직접|전언|추측","from":"","extent":"전부|부분","part":"","ref":"","q":""}], and "allow_to":[].`

// StructureRules follow the instructions on every generation call (1.3), an
// edited critic prompt included, as BodyRules do: the lists of their own
// (boundaries, closed), what stays (정체) apart from the one current state,
// and how two people's views of one event differ. Fold reads the lists.
const StructureRules = `
Structure (these come after the rules above and decide where they differ):
- 정체 (fact mode): what someone or something is and stays: true nature or species, the group or side they belong to, a title or role, a cover name or disguise and whose it is, what someone usually wears or keeps with them, what a thing is for and who holds it. One fact for each point. A change of state never replaces it; change it only by naming it in "was".
- Where a place is (정체, "slot":"위치"): for each place THIS TURN shows inside or part of another place (a room in a building, a building or quarter in a town, a floor of a tomb, a village in a region) or gives its direction and distance from another place, write one fact: "about" the place, "to" the place it is in or measured from, the body saying how they are joined ("푸른 언덕 마을에서 남쪽으로 반나절 거리"). Write it once; a place that moves or is renamed is changed by naming the fact in "was". What happens at a place now (a fight, a fire, who is there) is its 상태, not this.
- 상태 (fact mode): the current state of a person, place or thing, one fact for each aspect, with "slot" "지금/위치" (where they are now and what they are doing), "지금/몸" (body and condition now) or "지금/차림·소지" (what they have on or in hand right now). Write an aspect when THIS TURN shows it or changes it; an aspect not written stays as KNOWN shows it. Never put identity, group or names in it. Never put in it what stays beyond this moment (usual clothes, a mask or disguise worn as a role, things kept or owned, what a thing is for, a standing role or purpose) or what happened in THIS TURN: the first is a 정체 fact of its own, one for each point; the second belongs in the event's body. Do not write "was" for the earlier state of the same aspect; it is replaced for you. Counted values (money, numbers) stay in effects as before.
- "boundaries" (top-level array): one entry for each SECRET, not for each item. After writing the memories ask once: does THIS TURN show something someone hides, lies about, pretends, or does not know or is wrong about, that matters later (a true identity or nature, a disguise or false name, a cover story, a hidden plan or aim, a hidden act, a mistaken belief about these)? For each such secret that BOUNDARIES in KNOWN does not show yet, add one entry:
  {"on":"<id of the ONE item or name that states the secret: m1, n1, M12, N3, R2>","what":"","whose":"","knows":[],"unaware":[],"hide_from":[],"q":""}
  - Put it on the one item that says the secret itself, never on the other events, views or states that happen under it. Things said where only some people are present are not a secret by that alone: write an entry only when the text shows someone is kept from it, deceived about it, or does not know it.
  - No item states it: leave "on" empty, write "what" (the secret, one sentence) and "whose" (the id of whom it is about).
  - A secret BOUNDARIES in KNOWN already shows: write nothing, unless THIS TURN shows someone new kept from it or deceived about it, or someone learning it; then add an entry for that KNOWN item ("on":"M12") naming only those people (unaware, hide_from or knows).
  - knows: only the people the text shows knowing it when that matters (who was told, an accomplice); do not list everyone else.
  - knows, unaware and hide_from hold ids of people or groups. A group the text names ("the townspeople", "outsiders", "the guards") is one entry: add it to entities with type group when it is new. unaware: does not know. hide_from: must not be told. Name only whom the text shows.
- "closed" (top-level array). Read every line under OPEN in KNOWN and ask whether it still stands after THIS TURN. Close it when THIS TURN shows it done, cancelled, replaced or no longer possible, and also when it ended without being done in so many words: the story has gone past it (what was planned, ordered or waited for was overtaken by what happened instead; the scene only moving on to other things, or the work not being shown again, is not this), the time it was limited to has passed or the condition that ends it has been met ("until the report comes", "before dawn", "during this operation", "in this talk"), someone else or another event did it, or its result is now reported. Add {"ref":"<its id>","status":"fulfilled|missed|cancelled|changed|impossible|resolved","how":"<how it ended, one sentence>","q":""}: fulfilled when its aim was reached in any way, changed when another plan replaced it, missed when a one-time promise or task was left undone past its time, impossible when it can no longer be done. A line with several parts (buy food and find an inn; repair the gate and post guards) ends only when every part has ended; while a part is still to do, leave it open. A condition that starts or triggers it ("when the report comes, escort the messenger") being met does not end it. A standing rule, policy, role, duty, limit or long-term goal (how someone must address, behave, keep, hide or treat something from now on; what someone means to reach over time) does not end when someone breaks it, keeps it, carries it out once or reaches part of it, or when another arrangement is made beside it; it stays open until THIS TURN shows it lifted, replaced or no longer needed. Never close a line only because it is old or not mentioned in THIS TURN. Leave out the ones this turn does not end.
- Views of the same event. When two or more people take part in an event, ask: did they take it the same way? When the text shows they did not (one misreads the other, has a reason they hide, wants something else), write a view for each of them whose body says what this person thinks, wants or misreads that the other does not, and add "unlike":"<id of the other person's view, or of the event or fact it departs from>". A view says the person's own reading, reason or aim; do not only retell the event under their name.`

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
		field("body", n.Body)
		b.WriteString("\n")
	}
	for _, r := range sel.Relations {
		fmt.Fprintf(&b, "[%s] %s same_as %s (%s) T%d", r.ID, r.Name, r.SameAs, r.How, r.Turn)
		if r.Q != "" {
			fmt.Fprintf(&b, " %q", r.Q)
		}
		b.WriteString("\n")
	}
	memory := func(m KnownMemory) {
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
	for _, m := range sel.Memories {
		if !sel.Open[m.ID] {
			memory(m)
		}
	}
	if len(sel.Open) > 0 {
		// oldest first: sel.Memories is in stored order
		b.WriteString("OPEN (promises, shared goals and questions not closed yet):\n")
		for _, m := range sel.Memories {
			if sel.Open[m.ID] {
				memory(m)
			}
		}
	}
	for _, e := range sel.Effects {
		writeEffect(&b, e.Ref, e.Turn, e.Effect, e.Subject, superseded)
	}
	for _, l := range sel.Links {
		fmt.Fprintf(&b, "[%s] T%d %s %s %s\n", l.ID, l.Turn, l.From, l.Rel, l.To)
	}
	if sel.Sections && len(sel.Awareness) > 0 {
		b.WriteString("BOUNDARIES (who knows, does not know or must not be told, as recorded):\n")
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
	if len(sel.Present) > 0 {
		fmt.Fprintf(&b, "PRESENT (people in the current scene so far): %s\n", strings.Join(sel.Present, ", "))
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

var shownDate = regexp.MustCompile(`^(\d{1,4})[-/.](\d{1,2})[-/.](\d{1,2})(?:[ T](\d{1,2}):(\d{2})(?::\d{2})?)?$`)

// DateShown writes a turn's stored date for a delivered line
// (yyyy/mm/dd): each date as year/month/day (the year as written, a time
// kept as HH:MM), without the variable's name; names stay only when the
// chat's date variables hold different dates (each would be unclear
// alone). Text that is no date ("Day 3") stays as it is.
func DateShown(text string) string {
	if text == "" || !strings.Contains(text, " = ") {
		return text
	}
	type pair struct{ name, date string }
	var pairs []pair
	distinct := map[string]bool{}
	for _, part := range strings.Split(text, "; ") {
		name, value, ok := strings.Cut(part, " = ")
		if !ok {
			return text
		}
		date := value
		if m := shownDate.FindStringSubmatch(strings.TrimSpace(value)); m != nil {
			mo, _ := strconv.Atoi(m[2])
			d, _ := strconv.Atoi(m[3])
			date = fmt.Sprintf("%s/%02d/%02d", m[1], mo, d)
			if m[4] != "" {
				h, _ := strconv.Atoi(m[4])
				date += fmt.Sprintf(" %02d:%s", h, m[5])
			}
		}
		pairs = append(pairs, pair{name, date})
		distinct[date] = true
	}
	if len(distinct) == 1 {
		return pairs[0].date
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.name + " " + p.date
	}
	return strings.Join(parts, "; ")
}

// BodyRules is added to the critic's instructions only while body tracking
// is on (1.0 E09/E10, AC1's body_events rules): the model records body facts
// the text states and never calculates or guesses them.
const BodyRules = `
Body facts (only what THIS TURN's text states; write nothing otherwise):
At the top level you may add "body_events":[{"kind":"","who":"","when":"","scope":"","visibility":"","partners":[],"exposure":{"classification":"","partner_compatibility":"","contraception":"","method":""},"paternity":{"status":"","candidates":[],"unaware":[],"learns":[]},"outcome":"","child":"","q":""}].
- kind is one of period_start, pregnancy_confirmed, pregnancy_ended, recovery_started, recovery_ended, conception_exposure, emergency_contraception, child_named, paternity_learned, profile. who is the person's name id (an id from KNOWN or this answer's entities).
- profile: a named person's sex and species now, {"kind":"profile","who":"","sex":"female|male|other","species":"","q":""} (no when or scope).
  Write only what KNOWN's "body" for that person does not show yet, or what THIS TURN corrects; never repeat a value KNOWN shows.
  sex: she/her/그녀 referring to the person means female, he/him male; a name, clothes or speech style alone does not. species: what the person really is, only a non-human kind the text states (elf, demon, dragon) — never the kind a disguise, a transformation or a cover story shows; empty for a human, but write "human" when THIS TURN shows a kind KNOWN shows was wrong.
- period_start: a period starts in the text. pregnancy_confirmed: the text establishes a pregnancy. pregnancy_ended: birth, loss or end of a pregnancy. recovery_started / recovery_ended: recovery after it.
- conception_exposure: an act that could cause pregnancy happens in the text. partners are the other people's name ids. exposure.classification is potentially_conceiving, not_potentially_conceiving or unknown; partner_compatibility is compatible, incompatible or unknown — write unknown when the text does not say; contraception is present only when the text says it is used, else none; with present, exposure.method is condom, pill, withdrawal, iud, implant, injection, patch (a patch or a vaginal ring), magic (a spell, potion or other magical or supernatural way the story has) or other, empty when the text does not say which.
- emergency_contraception: the person takes emergency contraception (a morning-after pill) in the text.
- pregnancy_ended gives outcome: birth when the text tells a birth, loss when it tells a miscarriage, stillbirth or other loss, unknown when it does not say. With a birth, child is the child's name id when the text names the child, and paternity as below when the text says who the father is at the birth.
- pregnancy_confirmed gives paternity whenever the text says or shows who the father is or may be (a memory item about the father does not replace it; write it here too): candidates the father's name id (or the possible fathers' ids); status confirmed only when the story establishes the parent (not marriage, suspicion or closeness), candidates otherwise; unknown when none is named. paternity.unaware: the people the text shows not knowing who the father is (the mother too, when she does not know or believes another man is); leave it empty otherwise.
- child_named: a child she bore earlier is named in the text: who is the mother, child the child's name id, when the day that child was born (as KNOWN or the text gives it).
- paternity_learned: someone learns who the father of her pregnancy or child is (also long after the birth; write it as well as any memory item about it): who is the mother, child the child when it is a child already born, paternity.learns the people who learn it, paternity.status and candidates when the text says who the father is, and paternity.unaware the people the text shows still not knowing.
- when: only a calendar date (YYYY/MM/DD) the text gives, or how many days before or after this turn it happens ("3 days before", "2 days after"); leave it empty when it happens in this turn or the text gives neither. Never copy a day number the story uses ("day 100"). scope: flashback, planned or hypothetical when it is not happening now; leave it empty otherwise.
- visibility: public when other people in the scene openly know it, else private. A body fact never means anyone knows it.
- Never write probabilities, cycle days or due dates. Do not infer a pregnancy or a period from a delay, desire, mood or repetition. Do not invent symptoms or a birth.
- q is a short exact quote from THIS TURN.`
