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

// CompareVersions orders two version strings (<0, 0, >0). Real semver when
// both parse (so prereleases order correctly); otherwise numeric-segment
// fallback, with a parseable version always above an unparseable one.
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

// compareNumericSegments is the fallback for versions semver rejects: compares
// leading numeric segments, longer run of equals wins ("1.2" < "1.2.1").
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
