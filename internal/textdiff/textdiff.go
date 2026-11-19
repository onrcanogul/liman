// Package textdiff compares two texts line by line.
package textdiff

import (
	"strings"
)

// op is one line of a diff: its kind (' ', '-' or '+') and its text.
type op struct {
	kind byte
	text string
}

// Lines returns a line-by-line comparison of old and updated: lines only in
// old are prefixed with "-", lines only in updated with "+", common lines
// with a space. It returns "" when the two are identical.
//
// Unchanged lines are only shown when they are within context lines of a
// change; longer unchanged stretches are replaced by a single "..." line. A
// negative context shows every line.
func Lines(old, updated []byte, context int) string {
	if string(old) == string(updated) {
		return ""
	}

	ops := diff(splitLines(old), splitLines(updated))

	// keep[i] says whether line i is shown. Changed lines always are;
	// unchanged lines only near a change.
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if context < 0 {
			keep[i] = true
			continue
		}
		if o.kind == ' ' {
			continue
		}
		// Mark the change and its surroundings, clamped to the slice.
		for j := max(0, i-context); j <= min(len(ops)-1, i+context); j++ {
			keep[j] = true
		}
	}

	var out strings.Builder
	skipping := false
	for i, o := range ops {
		if !keep[i] {
			// Emit the marker once per hidden stretch, not once per line.
			if !skipping {
				out.WriteString("  ...\n")
				skipping = true
			}
			continue
		}
		skipping = false
		out.WriteByte(o.kind)
		out.WriteByte(' ')
		out.WriteString(o.text)
		out.WriteByte('\n')
	}
	return out.String()
}

// diff computes the edit script that turns a into b.
//
// The algorithm is the textbook longest common subsequence (LCS). It needs
// time and memory proportional to len(a) x len(b) lines, which is
// irrelevant for config files of a few dozen lines.
func diff(a, b []string) []op {
	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:]. The table is filled from the bottom right corner, so each
	// cell only depends on cells that are already known.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	// Walk the table from the top left, emitting a line at every step.
	var ops []op
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	// Whatever is left on either side has no counterpart.
	for ; i < len(a); i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
