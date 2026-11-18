package store

import (
	"fmt"
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
		fmt.Fprintf(&b, " | %s: %s", strings.ReplaceAll(a.Kind, "_", " "), nameOf(names, a.Who))
		if a.Via != "" {
			fmt.Fprintf(&b, " (%s", a.Via)
			if a.From != "" {
				fmt.Fprintf(&b, ", from %s", nameOf(names, a.From))
			}
			b.WriteString(")")
		}
		if a.Extent != "" {
			fmt.Fprintf(&b, " %s", a.Extent)
		}
		if a.Part != "" {
			fmt.Fprintf(&b, " %q", a.Part)
		}
	}
	return b.String()
}

func nameOf(names map[string]string, id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id
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
		if a.Via != "" || a.From != "" || a.Extent != "" || a.Part != "" {
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

// groupLines writes the lines at the given positions with their shared
// awareness grouped (see GroupAwareness). lines[i] ends with
// awarenessText(rendered[i].aware) when it shows any.
func groupLines(lines []string, rendered []renderedRecord, at []int, names map[string]string) []string {
	key := make([]string, len(at))
	bare := make([]string, len(at))
	count := map[string]int{}
	for j, i := range at {
		suffix := awarenessText(rendered[i].aware, names)
		g, ok := awarenessGroup(rendered[i].aware, names)
		if suffix == "" || !ok || !strings.HasSuffix(lines[i], suffix) {
			continue
		}
		key[j], bare[j] = g, strings.TrimSuffix(lines[i], suffix)
		count[g]++
	}
	// a group is written together only when that is shorter than its lines
	// as they were; otherwise each line keeps its own (compact) text
	size := func(l string) int { return len([]rune(l)) + 1 }
	header := func(k string) string { return "- [for the lines below] " + k }
	grouped := map[string]bool{}
	for k, n := range count {
		if n < 2 {
			continue
		}
		before, after := 0, size(header(k))
		for j, i := range at {
			if key[j] == k {
				before += size(lines[i])
				after += size("  " + bare[j])
			}
		}
		grouped[k] = after < before
	}
	var out []string
	done := map[string]bool{}
	for j, i := range at {
		k := key[j]
		switch {
		case k == "":
			out = append(out, lines[i])
		case !grouped[k]:
			if c := bare[j] + " | " + k; size(c) <= size(lines[i]) {
				out = append(out, c)
			} else {
				out = append(out, lines[i])
			}
		case !done[k]:
			done[k] = true
			out = append(out, header(k))
			for jj := j; jj < len(at); jj++ {
				if key[jj] == k {
					out = append(out, "  "+bare[jj])
				}
			}
		}
	}
	return out
}
