package phpdyn

import (
	"sort"
	"strconv"
	"strings"
)

// LineSet is a line table of one file: line → executed (true) or instrumentable but not executed (false). Lines absent from it
// are not instrumentable (or dead code).
type LineSet map[int]bool

// Union returns the union of a and b: a line executed in either is executed.
func (a LineSet) Union(b LineSet) LineSet {
	out := make(LineSet, len(a)+len(b))
	for l, e := range a {
		out[l] = e
	}
	for l, e := range b {
		out[l] = out[l] || e
	}
	return out
}

// parseRanges decodes "3-5,9" into lines; ok is false when malformed.
func parseRanges(s string, f func(int)) bool {
	if s == "" {
		return true
	}
	for _, r := range strings.Split(s, ",") {
		a, b, two := strings.Cut(r, "-")
		x, err := strconv.Atoi(a)
		if err != nil || x <= 0 {
			return false
		}
		y := x
		if two {
			if y, err = strconv.Atoi(b); err != nil || y < x || y-x > 1_000_000 {
				return false
			}
		}
		for l := x; l <= y; l++ {
			f(l)
		}
	}
	return true
}

// ParseLineTable decodes a record's line table "<executed ranges>|<not executed ranges>".
func ParseLineTable(s string) (LineSet, bool) {
	a, b, ok := strings.Cut(s, "|")
	if !ok {
		return nil, false
	}
	ls := LineSet{}
	if !parseRanges(b, func(l int) { ls[l] = false }) || !parseRanges(a, func(l int) { ls[l] = true }) {
		return nil, false
	}
	return ls, true
}

// ranges encodes sorted lines as "3-5,9".
func ranges(lines []int) string {
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		a := lines[i]
		for i+1 < len(lines) && lines[i+1] == lines[i]+1 {
			i++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(a))
		if lines[i] != a {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(lines[i]))
		}
	}
	return b.String()
}

// Format encodes the table as a record's line table.
func (a LineSet) Format() string {
	var ex, none []int
	for l, e := range a {
		if e {
			ex = append(ex, l)
		} else {
			none = append(none, l)
		}
	}
	sort.Ints(ex)
	sort.Ints(none)
	return ranges(ex) + "|" + ranges(none)
}
