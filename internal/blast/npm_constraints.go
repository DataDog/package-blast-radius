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
