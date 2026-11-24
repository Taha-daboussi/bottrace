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
	var keyA, keyB []string
	flush := func() {
		if cur != nil {
			cur.Kind = textKind(keyA, keyB)
			out = append(out, *cur)
			cur, keyA, keyB = nil, nil, nil
		}
	}
	ia, ib := 0, 0
	for _, o := range diffOps(la, lb, ka, kb) {
		switch o.Op {
		case ' ':
			flush()
			ia, ib = ia+1, ib+1
		case '~':
			flush()
			out = append(out, diffChange{Kind: "live", Before: []string{o.A}, After: []string{o.B}})
			ia, ib = ia+1, ib+1
		case '-':
			if cur == nil {
				cur = &diffChange{}
			}
			cur.Before, keyA = append(cur.Before, o.A), append(keyA, ka[ia])
			ia++
		case '+':
			if cur == nil {
				cur = &diffChange{}
			}
			cur.After, keyB = append(cur.After, o.B), append(keyB, kb[ib])
			ib++
		}
	}
	flush()
	return out
}

// textKind names a group of changed lines (given masked, so a live value does not count): added or removed lines,
// a change of words, or the same words with other formatting or other custom emoji.
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
// icon. The buttons left over pair as a rename when they have the same target, or else the same row and column;
// the rest are added or removed.
func buttonChanges(ra, rb [][]button, live liveMask) []diffChange {
	var out []diffChange
	for _, k := range buttonKinds(ra, rb, live) {
		if k == "order" || k == "layout" {
			out = append(out, diffChange{Kind: k, Before: gridLines(ra), After: gridLines(rb)})
		}
	}
	type cell struct {
		b    button
		r, c int
	}
	flat := func(rows [][]button) []cell {
		var out []cell
		for r, row := range rows {
			for c, b := range row {
				out = append(out, cell{b, r, c})
			}
		}
		return out
	}
	ba, bb := flat(ra), flat(rb)
	queue := map[string][]int{}
	for i, x := range ba {
		k := live.apply(x.b.Text)
		queue[k] = append(queue[k], i)
	}
	usedA, usedB := make([]bool, len(ba)), make([]bool, len(bb))
	for j, y := range bb {
		k := live.apply(y.b.Text)
		if q := queue[k]; len(q) > 0 {
			queue[k] = q[1:]
			usedA[q[0]], usedB[j] = true, true
			out = append(out, pairChanges(ba[q[0]].b, y.b)...)
		}
	}
	rename := func(same func(x, y cell) bool) {
		for j, y := range bb {
			if usedB[j] {
				continue
			}
			for i, x := range ba {
				if usedA[i] || !same(x, y) {
					continue
				}
				usedA[i], usedB[j] = true, true
				out = append(out, diffChange{Kind: "label", Before: []string{buttonLineText(x.b)}, After: []string{buttonLineText(y.b)}})
				renamed := x.b
				renamed.Text = y.b.Text
				for _, c := range pairChanges(renamed, y.b) {
					c.Before = []string{buttonLineText(x.b)}
					out = append(out, c)
				}
				break
			}
		}
	}
	rename(func(x, y cell) bool { return buttonTarget(x.b) == buttonTarget(y.b) && buttonTarget(x.b) != "other" })
	rename(func(x, y cell) bool { return x.r == y.r && x.c == y.c })
	for i, x := range ba {
		if !usedA[i] {
			out = append(out, diffChange{Kind: "button-removed", Before: []string{buttonLineText(x.b)}})
		}
	}
	for j, y := range bb {
		if !usedB[j] {
			out = append(out, diffChange{Kind: "button-added", After: []string{buttonLineText(y.b)}})
		}
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
