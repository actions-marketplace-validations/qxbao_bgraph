package lockfile

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A minimal node-semver: enough to decide, like yarn v1 does, whether a workspace's version satisfies a dependency
// range (semver.satisfies without includePrerelease). A range that is not a semver range at all (a dist-tag) satisfies
// nothing; a range this implementation cannot read is an error, so the caller fails closed.

type semVersion struct {
	major, minor, patch int
	pre                 []string
}

var reSemVersion = regexp.MustCompile(`^[=v\s]*([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseSemVersion(s string) (semVersion, bool) {
	m := reSemVersion.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return semVersion{}, false
	}
	var v semVersion
	var err error
	if v.major, err = strconv.Atoi(m[1]); err != nil {
		return v, false
	}
	if v.minor, err = strconv.Atoi(m[2]); err != nil {
		return v, false
	}
	if v.patch, err = strconv.Atoi(m[3]); err != nil {
		return v, false
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (a semVersion) compare(b semVersion) int {
	if c := cmpInt(a.major, b.major); c != 0 {
		return c
	}
	if c := cmpInt(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmpInt(a.patch, b.patch); c != 0 {
		return c
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		if x == y {
			continue
		}
		xn, yn := isNumeric(x), isNumeric(y)
		switch {
		case xn && yn:
			xi, _ := strconv.Atoi(x)
			yi, _ := strconv.Atoi(y)
			return cmpInt(xi, yi)
		case xn:
			return -1
		case yn:
			return 1
		case x < y:
			return -1
		default:
			return 1
		}
	}
	return cmpInt(len(a.pre), len(b.pre))
}

type semComparator struct {
	op string // ">=", ">", "<", "<=", "="
	v  semVersion
}

func (c semComparator) test(v semVersion) bool {
	r := v.compare(c.v)
	switch c.op {
	case ">=":
		return r >= 0
	case ">":
		return r > 0
	case "<":
		return r < 0
	case "<=":
		return r <= 0
	}
	return r == 0
}

// partial is a possibly incomplete version: missing or wildcard parts are -1.
type partial struct {
	major, minor, patch int
	pre                 []string
}

var rePartial = regexp.MustCompile(`^v?([0-9]+|[xX*])(?:\.([0-9]+|[xX*]))?(?:\.([0-9]+|[xX*]))?(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parsePartial(s string) (partial, bool) {
	if s == "" {
		return partial{-1, -1, -1, nil}, true
	}
	m := rePartial.FindStringSubmatch(s)
	if m == nil {
		return partial{}, false
	}
	p := partial{-1, -1, -1, nil}
	num := func(x string) int {
		if x == "" || x == "x" || x == "X" || x == "*" {
			return -1
		}
		n, err := strconv.Atoi(x)
		if err != nil {
			return -2
		}
		return n
	}
	p.major, p.minor, p.patch = num(m[1]), num(m[2]), num(m[3])
	if p.major == -2 || p.minor == -2 || p.patch == -2 {
		return partial{}, false
	}
	// A wildcard part makes every following part a wildcard.
	if p.major < 0 {
		p.minor, p.patch = -1, -1
	} else if p.minor < 0 {
		p.patch = -1
	}
	if m[4] != "" {
		if p.patch < 0 {
			return partial{}, false
		}
		p.pre = strings.Split(m[4], ".")
	}
	return p, true
}

func ver(major, minor, patch int, pre ...string) semVersion {
	return semVersion{major, minor, patch, pre}
}

// desugar turns one operator + partial version into comparators (node-semver's replaceTilde/Caret/XRange/hyphen).
func desugar(op string, p partial) ([]semComparator, error) {
	lowZero := []string{"0"} // "-0": the lowest prerelease, so that "<2.0.0-0" excludes 2.0.0 prereleases
	full := p.patch >= 0
	switch op {
	case "~", "~>":
		switch {
		case p.major < 0:
			return nil, nil
		case p.minor < 0:
			return []semComparator{{">=", ver(p.major, 0, 0)}, {"<", ver(p.major+1, 0, 0, lowZero...)}}, nil
		case !full:
			return []semComparator{{">=", ver(p.major, p.minor, 0)}, {"<", ver(p.major, p.minor+1, 0, lowZero...)}}, nil
		}
		return []semComparator{{">=", ver(p.major, p.minor, p.patch, p.pre...)}, {"<", ver(p.major, p.minor+1, 0, lowZero...)}}, nil
	case "^":
		switch {
		case p.major < 0:
			return nil, nil
		case p.minor < 0:
			return []semComparator{{">=", ver(p.major, 0, 0)}, {"<", ver(p.major+1, 0, 0, lowZero...)}}, nil
		case !full:
			if p.major == 0 {
				return []semComparator{{">=", ver(0, p.minor, 0)}, {"<", ver(0, p.minor+1, 0, lowZero...)}}, nil
			}
			return []semComparator{{">=", ver(p.major, p.minor, 0)}, {"<", ver(p.major+1, 0, 0, lowZero...)}}, nil
		}
		lo := semComparator{">=", ver(p.major, p.minor, p.patch, p.pre...)}
		switch {
		case p.major > 0:
			return []semComparator{lo, {"<", ver(p.major+1, 0, 0, lowZero...)}}, nil
		case p.minor > 0:
			return []semComparator{lo, {"<", ver(0, p.minor+1, 0, lowZero...)}}, nil
		}
		return []semComparator{lo, {"<", ver(0, 0, p.patch+1, lowZero...)}}, nil
	case "", "=":
		switch {
		case p.major < 0:
			return nil, nil
		case p.minor < 0:
			return []semComparator{{">=", ver(p.major, 0, 0)}, {"<", ver(p.major+1, 0, 0, lowZero...)}}, nil
		case !full:
			return []semComparator{{">=", ver(p.major, p.minor, 0)}, {"<", ver(p.major, p.minor+1, 0, lowZero...)}}, nil
		}
		return []semComparator{{"=", ver(p.major, p.minor, p.patch, p.pre...)}}, nil
	case ">", ">=", "<", "<=":
		if p.major < 0 {
			if op == "<" || op == ">" {
				return []semComparator{{"<", ver(0, 0, 0, lowZero...)}}, nil // matches nothing
			}
			return nil, nil
		}
		if full {
			return []semComparator{{op, ver(p.major, p.minor, p.patch, p.pre...)}}, nil
		}
		minor := max(p.minor, 0)
		switch op {
		case ">": // >1 → >=2.0.0; >1.2 → >=1.3.0
			if p.minor < 0 {
				return []semComparator{{">=", ver(p.major+1, 0, 0)}}, nil
			}
			return []semComparator{{">=", ver(p.major, p.minor+1, 0)}}, nil
		case ">=":
			return []semComparator{{">=", ver(p.major, minor, 0)}}, nil
		case "<":
			return []semComparator{{"<", ver(p.major, minor, 0, lowZero...)}}, nil
		case "<=": // <=1 → <2.0.0-0; <=1.2 → <1.3.0-0
			if p.minor < 0 {
				return []semComparator{{"<", ver(p.major+1, 0, 0, lowZero...)}}, nil
			}
			return []semComparator{{"<", ver(p.major, p.minor+1, 0, lowZero...)}}, nil
		}
	}
	return nil, fmt.Errorf("operator %q", op)
}

var reComparatorTok = regexp.MustCompile(`^(~>|~|\^|>=|<=|>|<|=)?\s*(\S*)$`)

// parseSemRange parses a range into sets of comparators (OR of ANDs).
func parseSemRange(r string) ([][]semComparator, error) {
	var out [][]semComparator
	for _, set := range strings.Split(r, "||") {
		set = strings.TrimSpace(set)
		var cs []semComparator
		if lo, hi, ok := strings.Cut(set, " - "); ok {
			pl, ok1 := parsePartial(strings.TrimSpace(lo))
			ph, ok2 := parsePartial(strings.TrimSpace(hi))
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("bad hyphen range %q", set)
			}
			a, err := desugar(">=", pl)
			if err != nil {
				return nil, err
			}
			b, err := desugar("<=", ph)
			if err != nil {
				return nil, err
			}
			out = append(out, append(a, b...))
			continue
		}
		// Join an operator separated from its version by spaces (">= 1.2.3").
		fields := strings.Fields(set)
		var toks []string
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			if (f == ">=" || f == "<=" || f == ">" || f == "<" || f == "=" || f == "~" || f == "^" || f == "~>") && i+1 < len(fields) {
				f += fields[i+1]
				i++
			}
			toks = append(toks, f)
		}
		for _, t := range toks {
			m := reComparatorTok.FindStringSubmatch(t)
			if m == nil {
				return nil, fmt.Errorf("bad comparator %q", t)
			}
			p, ok := parsePartial(m[2])
			if !ok {
				return nil, fmt.Errorf("bad version %q", m[2])
			}
			c, err := desugar(m[1], p)
			if err != nil {
				return nil, err
			}
			cs = append(cs, c...)
		}
		out = append(out, cs)
	}
	return out, nil
}

var reDistTag = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)

// semverSatisfies reports whether version satisfies the range (node-semver semantics, prereleases excluded unless a
// comparator of the same [major, minor, patch] has one). A dist-tag ("latest", "next") or an unparseable version
// satisfies nothing; a range that is neither a tag nor readable is an error.
func semverSatisfies(version, r string) (bool, error) {
	v, ok := parseSemVersion(version)
	if !ok {
		return false, nil
	}
	r = strings.TrimSpace(r)
	if reDistTag.MatchString(r) && r != "x" && r != "X" {
		return false, nil
	}
	sets, err := parseSemRange(r)
	if err != nil {
		return false, err
	}
	for _, set := range sets {
		ok := true
		for _, c := range set {
			if !c.test(v) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if len(v.pre) == 0 {
			return true, nil
		}
		for _, c := range set {
			if len(c.v.pre) > 0 && c.v.major == v.major && c.v.minor == v.minor && c.v.patch == v.patch {
				return true, nil
			}
		}
	}
	return false, nil
}
