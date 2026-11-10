// Package memory holds the AC2 memory items (generation contract v2,
// AC2-GEN-ITEMS-V2.md) as the model writes them and as Go stores them.
//
// The model writes temporary ids (n1, m1, s1, l1) for new items and refers
// to stored items by the persistent ids it was shown: names N<n>, memories
// M<n>, event effects M<n>.<k>, identity relations R<n>, scenes S<n>,
// links L<n>. Go issues every persistent id.
package memory

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Output is one generation response (one confirmed turn).
type Output struct {
	ElapsedDays ElapsedDays `json:"elapsed_days"`
	Entities    []Entity    `json:"entities,omitempty"`
	Memories    []Memory    `json:"memories,omitempty"`
	Scenes      []Scene     `json:"scenes,omitempty"`
	Links       []Link      `json:"links,omitempty"`
	// BodyEvents are written only when body tracking is on (1.0 E09/E10).
	BodyEvents []BodyEvent `json:"body_events,omitempty"`
}

// BodyEvent is a body fact the turn's text states about a person (AC1
// body_events): kind is period_start | pregnancy_confirmed |
// pregnancy_ended | recovery_started | recovery_ended | conception_exposure.
// Who and partners are name ids. When is the time the text gives (empty:
// this turn); scope is flashback | planned | hypothetical for an event that
// is not happening now. The model never gives probabilities.
type BodyEvent struct {
	Kind       string         `json:"kind"`
	Who        string         `json:"who"`
	When       string         `json:"when,omitempty"`
	Scope      string         `json:"scope,omitempty"`
	Visibility string         `json:"visibility,omitempty"` // public | private
	Partners   []string       `json:"partners,omitempty"`
	Exposure   *BodyExposure  `json:"exposure,omitempty"`
	Paternity  *BodyPaternity `json:"paternity,omitempty"`
	Q          string         `json:"q,omitempty"`
}

// BodyExposure is how the text describes a conception exposure.
type BodyExposure struct {
	Classification string `json:"classification,omitempty"`        // potentially_conceiving | not_potentially_conceiving | unknown
	Compatibility  string `json:"partner_compatibility,omitempty"` // compatible | incompatible | unknown
	Contraception  string `json:"contraception,omitempty"`         // none | present | unknown
}

// BodyPaternity is the parentage the text establishes for a pregnancy.
type BodyPaternity struct {
	Status     string   `json:"status,omitempty"` // confirmed | candidates | unknown
	Candidates []string `json:"candidates,omitempty"`
}

// ElapsedDays is the critic's turn-level passage of whole story days.
// An absent or unreadable value is zero and never invalidates memory items.
type ElapsedDays int64

func (d *ElapsedDays) UnmarshalJSON(b []byte) error {
	var n int64
	if json.Unmarshal(b, &n) != nil || n < 0 {
		n = 0
	}
	*d = ElapsedDays(n)
	return nil
}

// Awareness is who knows what, and what may be disclosed to whom. It can be
// attached to any memory, name, identity relation or link. unaware is
// knowledge (someone does not know); hide_from is a disclosure policy; they
// are kept apart.
type Awareness struct {
	Knows    []Knows `json:"knows,omitempty"`
	Unaware  Who     `json:"unaware,omitempty"`
	HideFrom Who     `json:"hide_from,omitempty"`
	AllowTo  Who     `json:"allow_to,omitempty"`
}

type Knows struct {
	Who    string `json:"who"`
	Via    string `json:"via,omitempty"`    // 직접 | 전언 | 추측
	From   string `json:"from,omitempty"`   // who told them
	Extent string `json:"extent,omitempty"` // 전부 | 부분
	Part   string `json:"part,omitempty"`   // which part, when partial
	Ref    string `json:"ref,omitempty"`    // evidence (event or utterance)
	Q      string `json:"q,omitempty"`
}

// Entity is a new name. same_as/how/relation state an identity relation
// recorded for this turn only; it is never re-applied by name later.
type Entity struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"` // person | place | thing | group
	Q        string `json:"q,omitempty"`
	SameAs   string `json:"same_as,omitempty"`
	How      string `json:"how,omitempty"`      // alias | disguise | true_name
	Relation string `json:"relation,omitempty"` // temp id of the identity relation
	Awareness
}

// Memory is one event, fact or view, or a re-mention ({ref, q}).
type Memory struct {
	// a re-mention or an awareness update of a stored item
	Ref string `json:"ref,omitempty"`

	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"` // event | fact | view
	Body string `json:"body,omitempty"`
	Q    string `json:"q,omitempty"`
	// Imp is 0..10; nil is "not judged", which differs from 0
	Imp   *float64 `json:"imp,omitempty"`
	At    string   `json:"at,omitempty"`
	Was   Refs     `json:"was,omitempty"`
	Scene string   `json:"scene,omitempty"`

	// event
	Roles   *Roles   `json:"roles,omitempty"`
	Time    string   `json:"time,omitempty"`
	Effects []Effect `json:"effects,omitempty"`
	Outcome string   `json:"outcome,omitempty"` // attempt | success | partial | failed | interrupted | cancelled; empty = not recorded
	Setting string   `json:"setting,omitempty"`
	SeenBy  Who      `json:"seen_by,omitempty"`

	// fact
	About  string `json:"about,omitempty"`
	Slot   string `json:"slot,omitempty"`
	Mode   string `json:"mode,omitempty"`
	To     Who    `json:"to,omitempty"` // one or several (a shared goal's members)
	Scope  string `json:"scope,omitempty"`
	Terms  string `json:"terms,omitempty"`
	Status string `json:"status,omitempty"`

	// view (About is shared with fact)
	Owner     string  `json:"owner,omitempty"`
	Aspect    string  `json:"aspect,omitempty"` // belief | memory | interpretation | emotion | motive | goal | intent
	Basis     *Basis  `json:"basis,omitempty"`
	Affect    *Affect `json:"affect,omitempty"`
	Expressed string  `json:"expressed,omitempty"`
	Differs   string  `json:"differs,omitempty"` // yes | no | unknown

	Awareness
}

// Roles of an event: the giver is the actor, the receiver the target.
type Roles struct {
	Actor    []string `json:"actor,omitempty"`
	Target   []string `json:"target,omitempty"`
	Object   []string `json:"object,omitempty"`
	Place    []string `json:"place,omitempty"`
	Speaker  []string `json:"speaker,omitempty"`
	Listener []string `json:"listener,omitempty"`
}

// Effect is one change an event caused: a value change or an obligation
// status change. Go gives each effect its own reference (M<n>.<k>).
type Effect struct {
	About  string `json:"about,omitempty"`
	Slot   string `json:"slot,omitempty"`
	Before string `json:"before,omitempty"`
	Value  string `json:"value,omitempty"`
	Delta  string `json:"delta,omitempty"`
	Was    Refs   `json:"was,omitempty"`
	Status string `json:"status,omitempty"`
}

// UnmarshalJSON reads before, value and delta written as a string or a
// number; a number is kept as written ("before": 65 is "65").
func (e *Effect) UnmarshalJSON(b []byte) error {
	type plain Effect
	v := struct {
		*plain
		Before scalar `json:"before,omitempty"`
		Value  scalar `json:"value,omitempty"`
		Delta  scalar `json:"delta,omitempty"`
	}{plain: (*plain)(e)}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	e.Before, e.Value, e.Delta = string(v.Before), string(v.Value), string(v.Delta)
	return nil
}

// scalar is a string, or a number kept as its JSON text.
type scalar string

func (s *scalar) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*s = scalar(one)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*s = scalar(n.String())
	return nil
}

type Basis struct {
	Via        string `json:"via,omitempty"`  // 경험 | 전언 | 추론 | 소문 | 기억 | 내면 서술
	From       string `json:"from,omitempty"` // the source the person claims
	Ref        string `json:"ref,omitempty"`
	Confidence string `json:"confidence,omitempty"`
}

// UnmarshalJSON reads a basis written as an object, or as a string naming
// only the way the person knows it ("basis": "경험" is via 경험); its from
// is one source or a list.
func (b *Basis) UnmarshalJSON(raw []byte) error {
	var via string
	if json.Unmarshal(raw, &via) == nil {
		*b = Basis{Via: via}
		return nil
	}
	type plain Basis
	v := struct {
		*plain
		From Who `json:"from,omitempty"`
	}{plain: (*plain)(b)}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	// the source may be written as a list ("from": ["N83"]); 2026-10-30 a
	// list dropped the whole basis of two views
	b.From = strings.Join(v.From, ",")
	return nil
}

type Affect struct {
	Intensity   string `json:"intensity,omitempty"`
	Persistence string `json:"persistence,omitempty"` // 순간 | 남음 | 누적 태도
}

// Scene marks a scene opening, continuing or closing; it has no content.
type Scene struct {
	ID     string `json:"id,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Change string `json:"change,omitempty"` // open | continue | close
	Q      string `json:"q,omitempty"`
}

// Link is a meaning link the model read in the text.
type Link struct {
	ID   string `json:"id,omitempty"`
	From string `json:"from"`
	Rel  string `json:"rel"` // causes | condition_of | changes | fulfills | recalls | contradicts | same_fact
	To   string `json:"to"`
	Q    string `json:"q,omitempty"`
	Awareness
}

// Refs is one reference or several; the model may write either.
type Refs []string

func (r *Refs) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		if one != "" {
			*r = Refs{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*r = many
	return nil
}

// Who is a list of people (ids or names). The model may write one name, a
// list of names, or a list of objects naming the person ("who" or "name");
// each gives the same list. Anything else is a shape error.
type Who []string

func (w *Who) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		if one != "" {
			*w = Who{one}
		}
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil {
		return err
	}
	out := Who{}
	for _, it := range items {
		var s string
		if json.Unmarshal(it, &s) == nil {
			out = append(out, s)
			continue
		}
		var o struct {
			Who  string `json:"who"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(it, &o); err != nil || (o.Who == "" && o.Name == "") {
			return fmt.Errorf("not a person: %s", string(it))
		}
		if o.Who != "" {
			out = append(out, o.Who)
		} else {
			out = append(out, o.Name)
		}
	}
	*w = out
	return nil
}
