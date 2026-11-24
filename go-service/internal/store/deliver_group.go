package store

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// GroupAwareness writes who knows, does not know or must not be told once
// for the lines that share it (1.3, 11-16 user: the same boundary repeated
// on every line). Lines whose shown awareness is the same are put together
// under one header line at the place of the first of them, without it on
// each line; a line no other shares keeps it on its line, each kind written
// once with its people listed. It only changes how the selected lines are
// written: selection and the counted chars are as before, and the text sent
// is never longer than the lines counted.
var GroupAwareness = true

// awarenessText is how a line shows its awareness (deliveryLine's suffix).
func awarenessText(aware []memory.KnownAwareness, names map[string]string) string {
	var b strings.Builder
	for _, a := range aware {
		b.WriteString(" | ")
		writeAwareness(&b, a, names, false)
	}
	return b.String()
}

func writeAwareness(b *strings.Builder, a memory.KnownAwareness, names map[string]string, dated bool) {
	b.WriteString(strings.ReplaceAll(a.Kind, "_", " "))
	if dated {
		fmt.Fprintf(b, " at T%d", a.Turn)
	}
	b.WriteString(": " + nameOf(names, a.Who))
	if a.Via != "" || (dated && a.From != "") {
		var origin []string
		if a.Via != "" {
			origin = append(origin, a.Via)
		}
		if a.From != "" {
			origin = append(origin, "from "+nameOf(names, a.From))
		}
		b.WriteString(" (" + strings.Join(origin, ", ") + ")")
	}
	if a.Extent != "" {
		fmt.Fprintf(b, " %s", a.Extent)
	}
	if a.Part != "" {
		fmt.Fprintf(b, " %q", a.Part)
	}
}

func nameOf(names map[string]string, id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id
}

func qualifiedAwareness(a memory.KnownAwareness) bool {
	return a.Via != "" || a.From != "" || a.Extent != "" || a.Part != ""
}

// awarenessGroup is the compact text of a line's awareness, people of one
// kind together ("unaware: A, B | hide from: C"), and whether it can stand
// apart from the line (no via, from, extent or part: those belong to one
// record's knowing).
func awarenessGroup(aware []memory.KnownAwareness, names map[string]string) (string, bool) {
	order := []string{"knows", "unaware", "hide_from", "allow_to"}
	label := map[string]string{"knows": "knows", "unaware": "unaware", "hide_from": "hide from", "allow_to": "allow to"}
	who := map[string][]string{}
	for _, a := range aware {
		if qualifiedAwareness(a) {
			return "", false
		}
		if _, ok := label[a.Kind]; !ok {
			return "", false
		}
		n := nameOf(names, a.Who)
		dup := false
		for _, x := range who[a.Kind] {
			dup = dup || x == n
		}
		if !dup {
			who[a.Kind] = append(who[a.Kind], n)
		}
	}
	// a person who does not know it and must not be told it is named once,
	// under both kinds
	hidden := map[string]bool{}
	for _, n := range who["hide_from"] {
		hidden[n] = true
	}
	var both, unaware, hide []string
	inBoth := map[string]bool{}
	for _, n := range who["unaware"] {
		if hidden[n] {
			both, inBoth[n] = append(both, n), true
		} else {
			unaware = append(unaware, n)
		}
	}
	for _, n := range who["hide_from"] {
		if !inBoth[n] {
			hide = append(hide, n)
		}
	}
	who["unaware"], who["hide_from"] = unaware, hide
	var parts []string
	for _, k := range order {
		if k == "unaware" && len(both) > 0 {
			parts = append(parts, "unaware, hide from: "+strings.Join(both, ", "))
		}
		if len(who[k]) > 0 {
			parts = append(parts, label[k]+": "+strings.Join(who[k], ", "))
		}
	}
	return strings.Join(parts, " | "), len(parts) > 0
}

type boundaryKey struct {
	targets string // stored IDs, even when the visible descriptions match
	header  string
}

type boundaryLabel struct {
	boundaryKey
	suffix, compact string
	record          string
	subjects        []boundarySubject
}

type boundarySubject struct {
	record, people, full string
}

func (label boundaryLabel) groupHeader(present map[string]bool) string {
	if len(label.subjects) == 0 {
		return label.header
	}
	var parts []string
	for _, s := range label.subjects {
		if present[s.record] {
			parts = append(parts, s.people)
		} else {
			parts = append(parts, s.full)
		}
	}
	return strings.Join(parts, " | ")
}

func plainBoundary(aware []memory.KnownAwareness, names map[string]string) boundaryLabel {
	compact, _ := awarenessGroup(aware, names)
	return boundaryLabel{boundaryKey: boundaryKey{header: compact}, suffix: awarenessText(aware, names), compact: compact}
}

// The caller supplies one target's stored rows before display deduplication.
// Date only states for which this person's earlier ignorance and later
// knowledge are both recorded. Keep the instruction to hide it independent.
func knowledgeBoundary(aware []memory.KnownAwareness, names map[string]string) boundaryLabel {
	firstUnaware, lastKnows := map[string]int{}, map[string]int{}
	for _, a := range aware {
		switch a.Kind {
		case "unaware":
			if a.Turn > 0 && (firstUnaware[a.Who] == 0 || a.Turn < firstUnaware[a.Who]) {
				firstUnaware[a.Who] = a.Turn
			}
		case "knows":
			lastKnows[a.Who] = max(lastKnows[a.Who], a.Turn)
		}
	}
	var ordinary, dated []memory.KnownAwareness
	for _, a := range aware {
		earlier := firstUnaware[a.Who]
		if (a.Kind == "knows" && earlier > 0 && earlier < a.Turn) ||
			(a.Kind == "unaware" && a.Turn > 0 && a.Turn < lastKnows[a.Who]) {
			dated = append(dated, a)
		} else {
			ordinary = append(ordinary, a)
		}
	}
	label := plainBoundary(distinctAwareness(ordinary), names)
	if len(dated) == 0 {
		return label
	}
	sort.SliceStable(dated, func(i, j int) bool { return dated[i].Turn < dated[j].Turn })
	groupable := len(ordinary) == 0 || label.header != ""
	var entries []string
	for _, a := range dated {
		groupable = groupable && !qualifiedAwareness(a)
		var b strings.Builder
		writeAwareness(&b, a, names, true)
		if entry := b.String(); !slices.Contains(entries, entry) {
			entries = append(entries, entry)
		}
	}
	history := strings.Join(entries, " | ")
	label.suffix += " | " + history
	if groupable {
		label.compact = strings.TrimPrefix(label.compact+" | "+history, " | ")
		label.header = label.compact
	} else {
		label.header, label.compact = "", ""
	}
	return label
}

// Keep each boundary's people and qualifiers with its own subject. On
// the source line the body already states it. An inherited boundary must
// still name its subject when the basis is rendered as a shared B mark.
func namedBoundary(r renderedRecord, names map[string]string, secret map[string]bool, bodies map[string]string) boundaryLabel {
	byTarget := map[string][]memory.KnownAwareness{}
	var targets []string
	for _, a := range r.carried {
		target := a.Target
		if !secret[target] {
			target = "" // ordinary awareness has no secret subject
		}
		if _, ok := byTarget[target]; !ok {
			targets = append(targets, target)
		}
		byTarget[target] = append(byTarget[target], a)
	}
	label := boundaryLabel{record: r.memory.ID}
	var inline, headers []string
	groupable := true
	for _, target := range targets {
		part := knowledgeBoundary(byTarget[target], names)
		if part.header == "" {
			groupable = false
		}
		what := bodies[target] // excludes muted records
		record, _, _ := strings.Cut(target, ".")
		people := part.header
		if what != "" {
			subject := " (what: " + what + ")"
			part.header += subject
			if record != r.memory.ID && what != r.memory.Body {
				part.suffix += subject
				part.compact += subject
			}
		}
		label.subjects = append(label.subjects, boundarySubject{record, people, part.header})
		label.suffix += part.suffix
		inline = append(inline, part.compact)
		headers = append(headers, part.header)
	}
	label.targets = strings.Join(targets, ",")
	if groupable {
		label.compact = strings.Join(inline, " | ")
		label.header = strings.Join(headers, " | ")
	}
	return label
}

// groupLines writes the lines at the given positions with their shared
// awareness grouped (see GroupAwareness).
func groupLines(lines []string, labels []boundaryLabel, at []int) []string {
	key := make([]boundaryKey, len(at))
	bare := make([]string, len(at))
	members := map[boundaryKey][]int{}
	for j, i := range at {
		label := labels[i]
		if label.header == "" || !strings.HasSuffix(lines[i], label.suffix) {
			continue
		}
		key[j], bare[j] = label.boundaryKey, strings.TrimSuffix(lines[i], label.suffix)
		members[key[j]] = append(members[key[j]], j)
	}
	// a group is written together only when that is shorter than its lines
	// as they were; otherwise each line keeps its own (compact) text
	size := func(l string) int { return len([]rune(l)) + 1 }
	const prefix = "- [for the lines below] "
	headers := map[boundaryKey]string{}
	for k, positions := range members {
		if len(positions) < 2 {
			continue
		}
		before, after := 0, size(prefix+k.header)
		present := map[string]bool{}
		for _, j := range positions {
			i := at[j]
			before += size(lines[i])
			after += size("  " + bare[j])
			present[labels[i].record] = true
		}
		if after < before {
			headers[k] = prefix + labels[at[positions[0]]].groupHeader(present)
		}
	}
	var out []string
	done := map[boundaryKey]bool{}
	for j, i := range at {
		k := key[j]
		switch {
		case k.header == "":
			out = append(out, lines[i])
		case headers[k] == "":
			if c := bare[j] + " | " + labels[i].compact; size(c) <= size(lines[i]) {
				out = append(out, c)
			} else {
				out = append(out, lines[i])
			}
		case !done[k]:
			done[k] = true
			out = append(out, headers[k])
			for _, jj := range members[k] {
				out = append(out, "  "+bare[jj])
			}
		}
	}
	return out
}
