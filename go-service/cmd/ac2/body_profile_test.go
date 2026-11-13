package main

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// Profiles (1.1): the critic records a person's sex and species; a woman is
// tracked with no body event, any species (AC1 character_deltas), and one
// person under several names is one body, not a card per name (AC1's
// duplicate cards).

// profileChat loads one turn per output into chat; each output is made from
// the name ids stored so far (name -> N id), as the model reads them in
// KNOWN.
func profileChat(t *testing.T, p *pipeline.Pipeline, db *store.DB, chat string, outputs ...func(ids map[string]string) string) map[string]string {
	t.Helper()
	var rows []string
	ids := map[string]string{}
	for i, out := range outputs {
		rows = append(rows, memRow(2*i, "user", "u"+string(rune('1'+i)), "", "다음"), memRow(2*i+1, "char", "a"+string(rune('1'+i)), "g"+string(rune('1'+i)), "이야기"))
		if _, err := p.Process([]observe.Record{attEvent(i+1, "output", chat, rows...)}); err != nil {
			t.Fatal(err)
		}
		var s *lifecycle.Session
		p.WithSession("chat:"+chat, func(v *lifecycle.Session) { s = v })
		own := memory.Own(s)
		u3Save(t, db, own[i].Source, out(ids))
		k, err := db.CurrentKnown(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range k.Names {
			ids[n.Name] = n.ID
		}
	}
	return ids
}

func bodyPeople(t *testing.T, p *pipeline.Pipeline, db *store.DB, chat string) []bodyPerson {
	t.Helper()
	rec := httptest.NewRecorder()
	bodyHandler(p, db)(rec, httptest.NewRequest("GET", "/body?chat_id="+chat, nil))
	var v struct {
		People []bodyPerson `json:"people"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(rec.Body.String(), err)
	}
	return v.People
}

func bodyOn(t *testing.T, p *pipeline.Pipeline, db *store.DB) {
	t.Helper()
	bodyCall(t, bodyHandler(p, db), "POST", "/body/settings", `{"cycle_tracking_enabled":true,"automatic_pregnancy_enabled":true}`, 200)
}

// A woman with only a profile is tracked on the model's start, her species
// shown; a man, a person of other sex and a person with no profile are not;
// the profile is no fact on the screen.
func TestBodyProfileTracksWoman(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	ids := profileChat(t, p, db, "P",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"릴리스","type":"person"},{"id":"n2","name":"철수","type":"person"},{"id":"n3","name":"아무개","type":"person"},{"id":"n4","name":"슬라임","type":"person"}],
			"memories":[{"id":"m1","kind":"event","body":"모였다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"여성","species":"악마","q":"그녀"},{"kind":"profile","who":"n2","sex":"male"},{"kind":"profile","who":"n4","sex":"other"}]}`
		},
		func(map[string]string) string {
			return `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"쉬었다"}]}`
		})
	people := bodyPeople(t, p, db, "P")
	if len(people) != 1 || people[0].Who != ids["릴리스"] || people[0].Name != "릴리스" {
		t.Fatalf("only the woman is tracked: %+v", people)
	}
	st := people[0].State
	if st.Cycle == nil || st.RefKind != "model_initialization" {
		t.Fatalf("tracked on the model's start: %+v", st)
	}
	if len(st.Lines) == 0 || !strings.Contains(st.Lines[0], "릴리스 [calculated, private, not known to anyone]: species 악마; ") {
		t.Fatalf("species shown: %q", st.Lines)
	}
	if len(people[0].Facts) != 0 {
		t.Fatalf("a profile is no body fact: %+v", people[0].Facts)
	}
	// KNOWN shows the profile, so the model does not record it again (the
	// generation's read of the earlier turns)
	var srcs []memory.Source
	p.WithSession("chat:P", func(v *lifecycle.Session) {
		for _, st := range memory.ScopeOf(v) {
			srcs = append(srcs, st.Source)
		}
	})
	k, err := db.KnownFor(srcs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range k.Names {
		got[n.Name] = n.Body
	}
	if got["릴리스"] != "female, 악마" || got["철수"] != "male" || got["아무개"] != "" {
		t.Fatalf("KNOWN profiles: %v", got)
	}
	sel := memory.Selection{Names: k.Names}
	if r := memory.RenderKnown(sel, nil); !strings.Contains(r, "릴리스 (person) T1 | body female, 악마") {
		t.Fatalf("rendered KNOWN: %s", r)
	}
}

// The latest profile in story order holds: a correction stops tracking a
// person with no body event; repeated profiles are one person.
func TestBodyProfileLatestHolds(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	profileChat(t, p, db, "Q",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"하나","type":"person"},{"id":"n2","name":"두리","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"만났다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female"},{"kind":"profile","who":"n2","sex":"female"}]}`
		},
		func(ids map[string]string) string {
			return `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"다시 만났다"}],
			"body_events":[{"kind":"profile","who":"` + ids["하나"] + `","sex":"female"},{"kind":"profile","who":"` + ids["두리"] + `","sex":"male"}]}`
		},
		func(ids map[string]string) string {
			return `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"헤어졌다"}],
			"body_events":[{"kind":"profile","who":"` + ids["하나"] + `","sex":"female","species":"엘프"}]}`
		})
	people := bodyPeople(t, p, db, "Q")
	if len(people) != 1 || people[0].Name != "하나" {
		t.Fatalf("one card for repeated profiles, none for the corrected one: %+v", people)
	}
	if !strings.Contains(people[0].State.Lines[0], "species 엘프; ") {
		t.Fatalf("the latest profile's species: %q", people[0].State.Lines)
	}
}

// One person under several names the story links is one body: an alias or
// a disguise is read as the name it is the same as, a description as the
// true name given; the facts recorded under each name are hers. Without
// the link they stay apart.
func TestBodyIdentityLinksOneCard(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	ids := profileChat(t, p, db, "R",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"세라 감마","type":"person"},{"id":"n2","name":"시골 소녀","type":"person"},{"id":"n3","name":"다른 소녀","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"만났다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female","species":"도플갱어"},{"kind":"profile","who":"n2","sex":"female"},{"kind":"profile","who":"n3","sex":"female"}]}`
		},
		func(ids map[string]string) string {
			return `{"elapsed_days":2,"entities":[{"id":"n1","name":"세라","type":"person","same_as":"` + ids["세라 감마"] + `","how":"alias"},{"id":"n2","name":"에린","type":"person","same_as":"` + ids["시골 소녀"] + `","how":"true_name"}],
			"memories":[{"id":"m1","kind":"event","body":"이름을 밝혔다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female"},{"kind":"period_start","who":"n1","q":"세라"},{"kind":"profile","who":"n2","sex":"female"}]}`
		})
	people := bodyPeople(t, p, db, "R")
	byName := map[string]bodyPerson{}
	for _, x := range people {
		if _, dup := byName[x.Name]; dup {
			t.Fatalf("two cards for %s: %+v", x.Name, people)
		}
		byName[x.Name] = x
	}
	if len(people) != 3 {
		t.Fatalf("세라 감마, 에린 and 다른 소녀 only: %+v", people)
	}
	nabe := byName["세라 감마"]
	if nabe.Who != ids["세라 감마"] || len(nabe.Facts) != 1 || nabe.Facts[0].Kind != "period_start" || nabe.State.RefKind != "observed_period_start" {
		t.Fatalf("the alias's period is hers: %+v", nabe)
	}
	if !strings.Contains(nabe.State.Lines[0], "species 도플갱어; ") {
		t.Fatalf("her species from the name kept: %q", nabe.State.Lines)
	}
	if byName["에린"].Who != ids["에린"] {
		t.Fatalf("a description is read as the true name: %+v", people)
	}
	if _, ok := byName["다른 소녀"]; !ok {
		t.Fatalf("a person with no link stays apart: %+v", people)
	}
}

// A model start chosen under a name before the story linked it to another
// stays the person's: after the link, and after her settings are changed
// on the screen.
func TestBodyIdentityLinkKeepsModelStart(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	var before string
	ids := profileChat(t, p, db, "T",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"시골 소녀","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"만났다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female"}]}`
		},
		func(ids map[string]string) string {
			// read before the link: the model start is chosen under the description
			people := bodyPeople(t, p, db, "T")
			if len(people) != 1 || people[0].State.RefKind != "model_initialization" {
				t.Fatalf("fixture: %+v", people)
			}
			before = people[0].State.CycleRef
			return `{"elapsed_days":1,"entities":[{"id":"n1","name":"에린","type":"person","same_as":"` + ids["시골 소녀"] + `","how":"true_name"}],"memories":[{"id":"m1","kind":"event","body":"이름을 밝혔다"}]}`
		})
	people := bodyPeople(t, p, db, "T")
	if len(people) != 1 || people[0].Who != ids["에린"] || people[0].State.CycleRef != before {
		t.Fatalf("the start chosen before the link (%s): %+v", before, people)
	}
	spec := people[0].Spec
	spec.PeriodDays = 6
	b, _ := json.Marshal(map[string]any{"chat_id": "T", "who": people[0].Who, "spec": spec})
	bodyCall(t, bodyHandler(p, db), "POST", "/body/person", string(b), 200)
	people = bodyPeople(t, p, db, "T")
	if len(people) != 1 || people[0].Spec.PeriodDays != 6 || people[0].State.CycleRef != before {
		t.Fatalf("settings changed, start kept (%s): %+v", before, people)
	}
}

// A relation kept from someone (hide_from or unaware) joins nothing: the
// body lines would show the link, so the two names stay apart as before
// and the hidden true name is not on the other's card.
func TestBodySecretIdentityStaysApart(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	ids := profileChat(t, p, db, "U",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"여행자","type":"person"},{"id":"n2","name":"기사","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"만났다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female"},{"kind":"period_start","who":"n1"}]}`
		},
		func(ids map[string]string) string {
			return `{"elapsed_days":1,"entities":[{"id":"n1","name":"릴리스","type":"person","same_as":"` + ids["여행자"] + `","how":"true_name","hide_from":["` + ids["기사"] + `"]}],"memories":[{"id":"m1","kind":"event","body":"정체를 숨겼다"}]}`
		})
	people := bodyPeople(t, p, db, "U")
	if len(people) != 1 || people[0].Who != ids["여행자"] || people[0].Name != "여행자" {
		t.Fatalf("a secret true name is not the card: %+v", people)
	}
	for _, l := range people[0].State.Lines {
		if strings.Contains(l, "릴리스") {
			t.Fatalf("the hidden name is in a body line: %q", l)
		}
	}
}

// Links in both directions and two true names given to one description
// are one person, read as one name.
func TestBodyLinkedNamesOnePerson(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	profileChat(t, p, db, "V",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"가희","type":"person"},{"id":"n2","name":"나희","type":"person"},{"id":"n3","name":"수상한 소녀","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"만났다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female"},{"kind":"profile","who":"n2","sex":"female"},{"kind":"profile","who":"n3","sex":"female"}]}`
		},
		func(ids map[string]string) string {
			return `{"elapsed_days":1,"entities":[{"id":"n1","name":"가희2","type":"person","same_as":"` + ids["가희"] + `","how":"alias"},{"id":"n2","name":"참이름","type":"person","same_as":"` + ids["수상한 소녀"] + `","how":"true_name"},{"id":"n3","name":"또다른이름","type":"person","same_as":"` + ids["수상한 소녀"] + `","how":"true_name"}],"memories":[{"id":"m1","kind":"event","body":"밝혔다"}]}`
		},
		func(ids map[string]string) string {
			// the reverse link: 가희 is the same as 가희2
			return `{"elapsed_days":1,"entities":[{"id":"n1","name":"가희3","type":"person","same_as":"` + ids["가희2"] + `","how":"alias"}],"memories":[{"id":"m1","kind":"event","body":"또 불렀다"}],
			"body_events":[{"kind":"period_start","who":"` + ids["가희2"] + `"}]}`
		})
	people := bodyPeople(t, p, db, "V")
	names := map[string]int{}
	for _, x := range people {
		names[x.Name]++
	}
	if len(people) != 3 || names["가희"] != 1 || names["나희"] != 1 || names["참이름"] != 1 {
		t.Fatalf("one card per person (가희, 나희, the description read as its first true name): %v", names)
	}
}

// A profile that is not the body now (hypothetical, planned, or dated
// before or after the turn) does not change the current body.
func TestBodyProfileNotNow(t *testing.T) {
	for _, tail := range []string{`,"scope":"hypothetical"`, `,"when":"2 days after"`} {
		db := modelsDB(t)
		p := pipeline.New(db)
		bodyOn(t, p, db)
		profileChat(t, p, db, "W",
			func(map[string]string) string {
				return `{"elapsed_days":0,"entities":[{"id":"n1","name":"변신자","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"변했다"}],
				"body_events":[{"kind":"profile","who":"n1","sex":"male"},{"kind":"profile","who":"n1","sex":"female"` + tail + `}]}`
			})
		if got := bodyPeople(t, p, db, "W"); len(got) != 0 {
			t.Fatalf("%s: a profile not of now changed the body: %+v", tail, got)
		}
	}
}

// The species is private: it is on the calculated line only, never on a
// story fact line labelled public.
func TestBodySpeciesNotOnPublicFact(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	profileChat(t, p, db, "X",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"여행자","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"알렸다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female","species":"숨긴악마"},{"kind":"pregnancy_confirmed","who":"n1","visibility":"public"}]}`
		})
	people := bodyPeople(t, p, db, "X")
	if len(people) != 1 {
		t.Fatalf("fixture: %+v", people)
	}
	for _, l := range people[0].State.Lines {
		if strings.Contains(l, "숨긴악마") && !strings.Contains(l, "[calculated, private") {
			t.Fatalf("species on a line not private: %q", l)
		}
	}
}

func scopeOf(p *pipeline.Pipeline, chat string) []memory.ScopedTurn {
	var scope []memory.ScopedTurn
	p.WithSession("chat:"+chat, func(v *lifecycle.Session) { scope = memory.ScopeOf(v) })
	return scope
}

// Delivery: every tracked person goes under the section's ceiling, the
// people the scene names (input or previous turn, by any of their names)
// first; no one is left out for not being named, a name inside another
// word only moves her up.
func TestBodyProfileDeliveredWhenNamed(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	profileChat(t, p, db, "Y",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"아델","type":"person"},{"id":"n2","name":"에린","type":"person"},{"id":"n3","name":"세라 감마","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"모였다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"female"},{"kind":"profile","who":"n2","sex":"female"},{"kind":"profile","who":"n3","sex":"female"},{"kind":"period_start","who":"n2"}]}`
		},
		func(ids map[string]string) string {
			return `{"elapsed_days":1,"entities":[{"id":"n1","name":"세라","type":"person","same_as":"` + ids["세라 감마"] + `","how":"Alias"}],"memories":[{"id":"m1","kind":"event","body":"불렀다"}]}`
		})
	if n := len(bodyPeople(t, p, db, "Y")); n != 3 {
		t.Fatalf("the screen shows every tracked person: %d", n)
	}
	order := func(scene string) []string {
		t.Helper()
		lines, err := bodyLines(db, "chat:Y", scopeOf(p, "Y"), 2, scene)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range lines {
			for _, n := range []string{"세라 감마", "아델", "에린"} {
				if strings.HasPrefix(l, "- body "+n+" ") && !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
		}
		return out
	}
	if got := order("세라, 이리 와. 이야기"); strings.Join(got, ",") != "세라 감마,아델,에린" {
		t.Fatalf("named by an alias first, no one left out: %v", got)
	}
	if got := order("그녀와 계속 이야기한다"); strings.Join(got, ",") != "아델,에린,세라 감마" {
		t.Fatalf("no one named: everyone in the screen's order: %v", got)
	}
}

// A person keeps her species through a disguise and its alias, under the
// name the user kept; a later correction under that name holds (each value
// the last profile gives, AC1; the critic records no apparent kind) — a
// combination once lost.
func TestBodyDisguiseKeepsOwnSpecies(t *testing.T) {
	for _, tc := range []struct {
		chat, want string
		correct    bool
	}{{"D1", "악마", false}, {"D2", "human", true}} {
		db := modelsDB(t)
		p := pipeline.New(db)
		bodyOn(t, p, db)
		turns := []func(map[string]string) string{
			func(map[string]string) string {
				return `{"elapsed_days":0,"entities":[{"id":"n1","name":"본모습","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"모였다"}],
				"body_events":[{"kind":"profile","who":"n1","sex":"female","species":"악마"}]}`
			},
			func(ids map[string]string) string {
				return `{"elapsed_days":1,"entities":[{"id":"n1","name":"가면","type":"person","same_as":"` + ids["본모습"] + `","how":"disguise"}],"memories":[{"id":"m1","kind":"event","body":"변장했다"}]}`
			},
			func(ids map[string]string) string {
				return `{"elapsed_days":1,"entities":[{"id":"n1","name":"가면별칭","type":"person","same_as":"` + ids["가면"] + `","how":"alias"}],"memories":[{"id":"m1","kind":"event","body":"불렀다"}]}`
			},
		}
		if tc.correct {
			turns = append(turns, func(ids map[string]string) string {
				return `{"elapsed_days":1,"memories":[{"id":"m1","kind":"event","body":"정체가 드러났다"}],"body_events":[{"kind":"profile","who":"` + ids["가면"] + `","species":"human"}]}`
			})
		}
		ids := profileChat(t, p, db, tc.chat, turns...)
		if _, err := db.MergeNames("chat:"+tc.chat, ids["가면"], ids["본모습"]); err != nil {
			t.Fatal(err)
		}
		people := bodyPeople(t, p, db, tc.chat)
		if len(people) != 1 || people[0].Who != ids["가면"] || len(people[0].Also) != 2 {
			t.Fatalf("%s: one person under the name the user kept: %+v", tc.chat, people)
		}
		if !strings.Contains(people[0].State.Lines[0], "species "+tc.want+"; ") {
			t.Fatalf("%s: species %s: %q", tc.chat, tc.want, people[0].State.Lines)
		}
	}
}

// KNOWN shows each name the profile of the person it is read as (an alias
// shows hers, so it is not recorded again), and nothing while tracking is
// off; a disguise's apparent species does not cover the person's own; how
// and sex are read in any case or spelling.
func TestBodyProfilePerPerson(t *testing.T) {
	db := modelsDB(t)
	p := pipeline.New(db)
	bodyOn(t, p, db)
	profileChat(t, p, db, "Z",
		func(map[string]string) string {
			return `{"elapsed_days":0,"entities":[{"id":"n1","name":"세라 감마","type":"person"}],"memories":[{"id":"m1","kind":"event","body":"모였다"}],
			"body_events":[{"kind":"profile","who":"n1","sex":"F","species":"도플갱어"}]}`
		},
		func(ids map[string]string) string {
			return `{"elapsed_days":1,"entities":[{"id":"n1","name":"세라","type":"person","same_as":"` + ids["세라 감마"] + `","how":" Disguise "}],"memories":[{"id":"m1","kind":"event","body":"변장했다"}],
			"body_events":[{"kind":"profile","who":"n1","species":"도플갱어"}]}`
		})
	people := bodyPeople(t, p, db, "Z")
	if len(people) != 1 || people[0].Name != "세라 감마" || len(people[0].Also) != 1 || people[0].Also[0] != "세라" {
		t.Fatalf("one person, the disguise shown as her other name: %+v", people)
	}
	if !strings.Contains(people[0].State.Lines[0], "species 도플갱어; ") {
		t.Fatalf("her own species: %q", people[0].State.Lines)
	}
	var srcs []memory.Source
	for _, st := range scopeOf(p, "Z") {
		srcs = append(srcs, st.Source)
	}
	k, err := db.KnownFor(srcs)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range k.Names {
		if n.Body != "female, 도플갱어" {
			t.Fatalf("%s shows %q", n.Name, n.Body)
		}
	}
	bodyCall(t, bodyHandler(p, db), "POST", "/body/settings", `{"cycle_tracking_enabled":false,"automatic_pregnancy_enabled":false}`, 200)
	if k, err = db.KnownFor(srcs); err != nil {
		t.Fatal(err)
	}
	for _, n := range k.Names {
		if n.Body != "" {
			t.Fatalf("tracking off: %s shows %q", n.Name, n.Body)
		}
	}
}
