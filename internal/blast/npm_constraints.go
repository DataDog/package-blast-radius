package blast

import (
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
)

// constraintCache avoids re-parsing the same version range string millions of times.
// Many packages use identical ranges like "^1.0.0" or ">=2.0.0".
var constraintCache sync.Map // string -> *semver.Constraints or nil
var versionCache sync.Map    // string -> *semver.Version or nil

// Caching whole (constraint, version) results looks tempting — npmMatches is
// called hundreds of millions of times in a deep run — but it doesn't pay:
// measured over `analyze npm keyv 6.0.0 --depth 5`, 14.35M calls covered 13.85M
// distinct pairs (~1.0x repetition). The parse caches above are worth keeping
// because range *strings* repeat heavily; the (range, version) pairs don't.

func getCachedVersion(s string) (*semver.Version, bool) {
	if v, ok := versionCache.Load(s); ok {
		if v == nil {
			return nil, false
		}
		return v.(*semver.Version), true
	}
	v, err := semver.NewVersion(s)
	if err != nil {
		versionCache.Store(s, nil)
		return nil, false
	}
	versionCache.Store(s, v)
	return v, true
}

func getCachedConstraint(s string) (*semver.Constraints, bool) {
	if v, ok := constraintCache.Load(s); ok {
		if v == nil {
			return nil, false
		}
		return v.(*semver.Constraints), true
	}
	c, err := semver.NewConstraint(s)
	if err != nil {
		constraintCache.Store(s, nil)
		return nil, false
	}
	constraintCache.Store(s, c)
	return c, true
}

func npmMatches(constraintStr string, version string) bool {
	constraintStr = strings.TrimSpace(constraintStr)
	if constraintStr == "latest" {
		return true
	}
	if constraintStr == "" {
		constraintStr = "*"
	}

	if strings.HasPrefix(constraintStr, "git") ||
		strings.HasPrefix(constraintStr, "http") ||
		strings.HasPrefix(constraintStr, "file:") ||
		strings.HasPrefix(constraintStr, "/") {
		return false
	}

	constraintStr = convertHyphenRange(constraintStr)

	c, ok := getCachedConstraint(constraintStr)
	if !ok {
		return false
	}

	v, ok := getCachedVersion(version)
	if !ok {
		return false
	}

	return c.Check(v)
}

func convertHyphenRange(s string) string {
	parts := strings.SplitN(s, " - ", 2)
	if len(parts) != 2 {
		return s
	}
	left := strings.TrimSpace(parts[0])
	right := strings.TrimSpace(parts[1])
	if left == "" || right == "" {
		return s
	}
	return ">= " + left + ", <= " + right
}
