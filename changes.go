package main

import (
	"fmt"
	"regexp"
	"strings"
)

// diffChange is one change between two states of a screen, with the exact lines on each side, so a judge can
// point at it by id and a report can quote it without typing it again.
type diffChange struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

var anyTag = regexp.MustCompile(`<[^<>]*>`)

// screenChanges lists the changes from a to b: groups of changed text lines, then the media, then each button
// matched by its label. Ids are left to the caller.
func screenChanges(a, b msgState, live liveMask) []diffChange {
	var out []diffChange
	out = append(out, textChanges(a, b, live)...)
	if (a.Media == nil) != (b.Media == nil) || (a.Media != nil && (a.Media.Kind != b.Media.Kind || a.Media.UniqueID != b.Media.UniqueID)) {
		out = append(out, diffChange{Kind: "media", Before: mediaLine(a), After: mediaLine(b)})
	}
	return append(out, buttonChanges(a.Rows, b.Rows, live)...)
}

func mediaLine(st msgState) []string {
	if st.Media == nil {
		return nil
	}
	return []string{fmt.Sprintf("[%s %s]", st.Media.Kind, st.Media.UniqueID)}
}

func textChanges(a, b msgState, live liveMask) []diffChange {
	la := strings.Split(strings.TrimRight(markup(a, true), "\n"), "\n")
	lb := strings.Split(strings.TrimRight(markup(b, true), "\n"), "\n")
	ka, kb := make([]string, len(la)), make([]string, len(lb))
	for i, l := range la {
		ka[i] = live.applyMarkup(l)
	}
	for i, l := range lb {
		kb[i] = live.applyMarkup(l)
	}
	var out []diffChange
	var cur *diffChange
	flush := func() {
		if cur != nil {
			cur.Kind = textKind(cur.Before, cur.After)
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, o := range diffOps(la, lb, ka, kb) {
		switch o.Op {
		case ' ':
			flush()
		case '~':
			flush()
			out = append(out, diffChange{Kind: "live", Before: []string{o.A}, After: []string{o.B}})
		case '-':
			if cur == nil {
				cur = &diffChange{}
			}
			cur.Before = append(cur.Before, o.A)
		case '+':
			if cur == nil {
				cur = &diffChange{}
			}
			cur.After = append(cur.After, o.B)
		}
	}
	flush()
	return out
}

// textKind names a group of changed lines: added or removed lines, a change of words, or the same words with
// other formatting or other custom emoji.
func textKind(before, after []string) string {
	switch {
	case len(before) == 0:
		return "line-added"
	case len(after) == 0:
		return "line-removed"
	}
	plain := func(ls []string) string { return anyTag.ReplaceAllString(strings.Join(ls, "\n"), "") }
	if plain(before) != plain(after) {
		return "text"
	}
	if emojiTag.ReplaceAllString(strings.Join(before, "\n"), "") != emojiTag.ReplaceAllString(strings.Join(after, "\n"), "") {
		return "format"
	}
	return "emoji"
}

func buttonLineText(b button) string {
	s := b.Text + " → " + buttonTarget(b)
	if b.Icon != "" {
		s += " · icon " + b.Icon
	}
	if b.Style != "" {
		s += " · " + b.Style
	}
	return s
}

// buttonChanges matches the buttons of a and b by label, in order: a matched pair can change its target, style or
// icon; an unmatched button is added or removed, or renamed when both sides have the same number left over.
func buttonChanges(ra, rb [][]button, live liveMask) []diffChange {
	var out []diffChange
	for _, k := range buttonKinds(ra, rb, live) {
		if k == "order" || k == "layout" {
			out = append(out, diffChange{Kind: k, Before: gridLines(ra), After: gridLines(rb)})
		}
	}
	var ba, bb []button
	for _, r := range ra {
		ba = append(ba, r...)
	}
	for _, r := range rb {
		bb = append(bb, r...)
	}
	queue := map[string][]int{}
	for i, x := range ba {
		k := live.apply(x.Text)
		queue[k] = append(queue[k], i)
	}
	usedA := make([]bool, len(ba))
	var leftB []int
	for j, y := range bb {
		k := live.apply(y.Text)
		q := queue[k]
		if len(q) == 0 {
			leftB = append(leftB, j)
			continue
		}
		i := q[0]
		queue[k] = q[1:]
		usedA[i] = true
		out = append(out, pairChanges(ba[i], y)...)
	}
	var leftA []int
	for i, u := range usedA {
		if !u {
			leftA = append(leftA, i)
		}
	}
	if len(leftA) == len(leftB) {
		for n := range leftA {
			x, y := ba[leftA[n]], bb[leftB[n]]
			out = append(out, diffChange{Kind: "label", Before: []string{buttonLineText(x)}, After: []string{buttonLineText(y)}})
			x.Text = y.Text
			for _, c := range pairChanges(x, y) {
				c.Before = []string{buttonLineText(ba[leftA[n]])}
				out = append(out, c)
			}
		}
		return out
	}
	for _, i := range leftA {
		out = append(out, diffChange{Kind: "button-removed", Before: []string{buttonLineText(ba[i])}})
	}
	for _, j := range leftB {
		out = append(out, diffChange{Kind: "button-added", After: []string{buttonLineText(bb[j])}})
	}
	return out
}

func pairChanges(x, y button) []diffChange {
	var out []diffChange
	add := func(kind string) {
		out = append(out, diffChange{Kind: kind, Before: []string{buttonLineText(x)}, After: []string{buttonLineText(y)}})
	}
	if buttonTarget(x) != buttonTarget(y) {
		add("callback")
	}
	if x.Style != y.Style {
		add("style")
	}
	if x.Icon != y.Icon {
		add("icon")
	}
	if x.Text != y.Text {
		add("live")
	}
	return out
}

func gridLines(rows [][]button) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(r))
		for j, b := range r {
			cells[j] = "[ " + b.Text + " ]"
		}
		out[i] = strings.Join(cells, " ")
	}
	return out
}
