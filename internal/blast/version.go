package blast

import (
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ParseVersion parses a version string as semver, sharing the package-level
// cache with the constraint matcher. Registry data contains plenty of strings
// that are not valid semver, so callers must handle ok == false.
func ParseVersion(s string) (*semver.Version, bool) {
	return getCachedVersion(s)
}

// CompareVersions orders two version strings, returning <0, 0 or >0.
//
// Real semver comparison is used when both sides parse, so prereleases order
// correctly (1.0.0-beta < 1.0.0). Registry versions that are not valid semver
// fall back to a numeric-segment comparison, and a parseable version always
// sorts above an unparseable one so the junk collects at one end.
func CompareVersions(a, b string) int {
	va, aOK := getCachedVersion(a)
	vb, bOK := getCachedVersion(b)

	switch {
	case aOK && bOK:
		return va.Compare(vb)
	case aOK:
		return 1
	case bOK:
		return -1
	default:
		return compareNumericSegments(a, b)
	}
}

// compareNumericSegments is the best-effort fallback for versions semver
// rejects. It compares the leading numeric segments and treats a longer run of
// equal segments as greater, so "1.2" sorts below "1.2.1".
func compareNumericSegments(a, b string) int {
	pa := numericSegments(a)
	pb := numericSegments(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] - pb[i]
		}
	}
	return len(pa) - len(pb)
}

func numericSegments(v string) []int {
	v = strings.TrimPrefix(v, "v")
	var parts []int
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' }) {
		n, err := strconv.Atoi(seg)
		if err != nil {
			break
		}
		parts = append(parts, n)
	}
	return parts
}
