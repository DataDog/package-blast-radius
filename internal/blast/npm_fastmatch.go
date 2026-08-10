package blast

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
)

// fastConstraint is a bool-only replacement for semver.Constraints.Check. The
// library's Check creates an fmt.Errorf on every non-match (30% of CPU in deep
// runs) and discards it; this mirrors the logic without the allocation.
// Anything the parser can't handle falls back to the library.

type fastPart struct {
	op         string // "", "=", "!=", ">", "<", ">=", "<=", "~", "^"
	con        *semver.Version
	dirty      bool
	minorDirty bool
	patchDirty bool
}

type fastGroup struct {
	parts       []fastPart
	containsPre bool
}

type fastConstraint struct {
	groups   []fastGroup
	fallback *semver.Constraints
}

func (fc *fastConstraint) check(v *semver.Version) bool {
	if fc == nil {
		return false
	}
	if fc.fallback != nil {
		return fc.fallback.Check(v)
	}
	for i := range fc.groups {
		g := &fc.groups[i]
		matched := true
		for j := range g.parts {
			if !g.parts[j].check(v, g.containsPre) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (p fastPart) check(v *semver.Version, includePre bool) bool {
	if v.Prerelease() != "" && !includePre {
		return false
	}
	switch p.op {
	case "", "=":
		return p.checkTildeOrEqual(v)
	case "!=":
		return p.checkNotEqual(v, includePre)
	case ">":
		return p.checkGreaterThan(v)
	case "<":
		return p.checkLessThan(v)
	case ">=":
		return p.checkGreaterThanEqual(v)
	case "<=":
		return p.checkLessThanEqual(v)
	case "~":
		return p.checkTilde(v)
	case "^":
		return p.checkCaret(v)
	}
	return false
}

func (p fastPart) checkTildeOrEqual(v *semver.Version) bool {
	if p.dirty {
		return p.checkTilde(v)
	}
	return v.Equal(p.con)
}

func (p fastPart) checkNotEqual(v *semver.Version, includePre bool) bool {
	if p.dirty {
		if p.con.Major() != v.Major() {
			return true
		}
		if p.con.Minor() != v.Minor() && !p.minorDirty {
			return true
		} else if p.minorDirty {
			return false
		} else if p.con.Patch() != v.Patch() && !p.patchDirty {
			return true
		} else if p.patchDirty {
			if v.Prerelease() != "" || p.con.Prerelease() != "" {
				return comparePrereleaseFast(v.Prerelease(), p.con.Prerelease()) != 0
			}
			return false
		}
	}
	return !v.Equal(p.con)
}

func (p fastPart) checkGreaterThan(v *semver.Version) bool {
	if !p.dirty {
		return v.Compare(p.con) == 1
	}
	if v.Major() > p.con.Major() {
		return true
	} else if v.Major() < p.con.Major() {
		return false
	} else if p.minorDirty {
		return false
	} else if p.patchDirty {
		return v.Minor() > p.con.Minor()
	}
	return v.Compare(p.con) == 1
}

func (p fastPart) checkLessThan(v *semver.Version) bool {
	return v.Compare(p.con) < 0
}

func (p fastPart) checkGreaterThanEqual(v *semver.Version) bool {
	return v.Compare(p.con) >= 0
}

func (p fastPart) checkLessThanEqual(v *semver.Version) bool {
	if !p.dirty {
		return v.Compare(p.con) <= 0
	}
	if v.Major() > p.con.Major() {
		return false
	}
	if v.Major() == p.con.Major() && v.Minor() > p.con.Minor() && !p.minorDirty {
		return false
	}
	return true
}

func (p fastPart) checkTilde(v *semver.Version) bool {
	if v.LessThan(p.con) {
		return false
	}
	if p.con.Major() == 0 && p.con.Minor() == 0 && p.con.Patch() == 0 &&
		!p.minorDirty && !p.patchDirty {
		return true
	}
	if v.Major() != p.con.Major() {
		return false
	}
	if v.Minor() != p.con.Minor() && !p.minorDirty {
		return false
	}
	return true
}

func (p fastPart) checkCaret(v *semver.Version) bool {
	if v.LessThan(p.con) {
		return false
	}
	if p.con.Major() > 0 || p.minorDirty {
		return v.Major() == p.con.Major()
	}
	if v.Major() > 0 {
		return false
	}
	if p.con.Minor() > 0 || p.patchDirty {
		return v.Minor() == p.con.Minor()
	}
	if v.Minor() > 0 {
		return false
	}
	return p.con.Patch() == v.Patch()
}

func comparePrereleaseFast(v, o string) int {
	if v == o {
		return 0
	}
	sparts := strings.Split(v, ".")
	oparts := strings.Split(o, ".")
	l := len(sparts)
	if len(oparts) > l {
		l = len(oparts)
	}
	for i := 0; i < l; i++ {
		sp, op := "", ""
		if i < len(sparts) {
			sp = sparts[i]
		}
		if i < len(oparts) {
			op = oparts[i]
		}
		if d := comparePrePartFast(sp, op); d != 0 {
			return d
		}
	}
	return 0
}

func comparePrePartFast(s, o string) int {
	if s == o {
		return 0
	}
	if s == "" {
		return -1
	}
	if o == "" {
		return 1
	}
	si, err1 := strconv.ParseUint(s, 10, 64)
	oi, err2 := strconv.ParseUint(o, 10, 64)
	if err1 != nil && err2 != nil {
		if s > o {
			return 1
		}
		return -1
	} else if err1 != nil {
		return -1
	} else if err2 != nil {
		return 1
	}
	if si > oi {
		return 1
	}
	return -1
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

var fastConstraintRegex = regexp.MustCompile(
	`(` + fastOps + `)\s*` + fastVersionPattern)

const fastOps = `>=|=>|<=|=<|!=|~>|~|\^|>|<|=|`

const fastVersionPattern = `v?([0-9xX*]+)(\.[0-9xX*]+)?(\.[0-9xX*]+)?` +
	`(-(?:[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?` +
	`(\+(?:[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?`

var fastConstraintCache sync.Map

func getCachedFastConstraint(s string) *fastConstraint {
	if v, ok := fastConstraintCache.Load(s); ok {
		return v.(*fastConstraint)
	}
	fc := parseFastConstraint(s)
	fastConstraintCache.Store(s, fc)
	return fc
}

func parseFastConstraint(s string) *fastConstraint {
	// Validate with the library first: it enforces length limits, OR group
	// count, separator rules, and rejects malformed input the fast regex might
	// accept (e.g. ">=1.0.0<2.0.0" with no separator, or "^1 ||" with an empty
	// OR group). This runs once per distinct constraint string (cached), so the
	// cost is negligible vs the millions of check calls it gates.
	c, err := semver.NewConstraint(s)
	if err != nil {
		return &fastConstraint{}
	}

	ors := strings.Split(s, "||")
	groups := make([]fastGroup, len(ors))
	for i, or := range ors {
		g, ok := parseFastGroup(or)
		if !ok {
			return &fastConstraint{fallback: c}
		}
		groups[i] = g
	}
	return &fastConstraint{groups: groups}
}

func parseFastGroup(s string) (fastGroup, bool) {
	matches := fastConstraintRegex.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		stripped := strings.TrimSpace(s)
		if stripped == "" || stripped == "*" {
			v, err := semver.NewVersion("0.0.0")
			if err != nil {
				return fastGroup{}, false
			}
			return fastGroup{parts: []fastPart{{op: "", con: v, dirty: true}}}, true
		}
		return fastGroup{}, false
	}

	// Guard against substring matches: "next" contains "x" which the regex
	// reads as a wildcard. Require the matches to cover the full input.
	var matched strings.Builder
	for _, m := range matches {
		matched.WriteString(m[0])
	}
	if stripSpaces(matched.String()) != stripSpaces(s) {
		return fastGroup{}, false
	}

	parts := make([]fastPart, 0, len(matches))
	containsPre := false
	for _, m := range matches {
		p, ok, hasPre := parseFastPart(m[1], m[2:])
		if !ok {
			return fastGroup{}, false
		}
		parts = append(parts, p)
		if hasPre {
			containsPre = true
		}
	}
	return fastGroup{parts: parts, containsPre: containsPre}, true
}

// parseFastPart parses one (operator, version) pair. ver submatches:
// ver[0]=major, ver[1]=".minor", ver[2]=".patch", ver[3]="-prerelease", ver[4]="+build"
func parseFastPart(op string, ver []string) (p fastPart, ok bool, hasPre bool) {
	// Normalize aliases the library maps to the same constraint function.
	switch op {
	case "~>":
		op = "~"
	case "=>":
		op = ">="
	case "=<":
		op = "<="
	}

	major := ver[0]
	minor := strings.TrimPrefix(ver[1], ".")
	patch := strings.TrimPrefix(ver[2], ".")
	pre := strings.TrimPrefix(ver[3], "-")

	var conStr string
	p.op = op

	switch {
	case isX(major) || major == "":
		p.dirty = true
		conStr = "0.0.0"
	case isX(minor) || minor == "":
		p.dirty = true
		p.minorDirty = true
		conStr = major + ".0.0"
	case isX(patch) || patch == "":
		p.dirty = true
		p.patchDirty = true
		conStr = major + "." + minor + ".0"
	default:
		conStr = major + "." + minor + "." + patch
	}
	if pre != "" {
		conStr += "-" + pre
	}

	v, err := semver.NewVersion(conStr)
	if err != nil {
		return fastPart{}, false, false
	}
	p.con = v
	return p, true, pre != ""
}

func isX(s string) bool { return s == "x" || s == "X" || s == "*" }

func stripSpaces(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != ' ' && r != '\t' && r != ',' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
