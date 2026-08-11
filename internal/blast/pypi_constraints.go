package blast

import (
	"strings"
	"sync"

	pep440 "github.com/aquasecurity/go-pep440-version"
)

var pypiSpecifierCache sync.Map // string -> *pep440.Specifiers or nil
var pypiVersionCache sync.Map   // string -> *pep440.Version or nil

func pypiMatches(requirement, version string) bool {
	spec, ok := pypiSpecifier(requirement)
	if !ok {
		return false
	}

	v, ok := getCachedPyPIVersion(version)
	if !ok {
		return false
	}

	// Python's default resolver excludes prereleases unless the specifier itself
	// opts into them. The matcher cannot know the full candidate set, so it uses
	// the conservative default for compromised prerelease targets.
	if v.IsPreRelease() && !pypiSpecMentionsPrerelease(spec) {
		return false
	}

	ss, ok := getCachedPyPISpecifier(spec)
	if !ok {
		return false
	}
	return ss.Check(*v)
}

func pypiSpecifier(requirement string) (string, bool) {
	requirement = strings.TrimSpace(requirement)
	if requirement == "" || requirement == "*" {
		return ">=0", true
	}

	// deps.dev may preserve PEP 508 markers in Requirement. For blast radius we
	// keep the edge because the marker could apply in a matching environment.
	if marker := strings.Index(requirement, ";"); marker >= 0 {
		requirement = strings.TrimSpace(requirement[:marker])
	}
	requirement = strings.TrimSpace(strings.Trim(requirement, "()"))
	if requirement == "" || requirement == "*" {
		return ">=0", true
	}
	if strings.Contains(requirement, " @ ") ||
		hasAnyPrefix(requirement, "git+", "http://", "https://", "file:", "file://", "/", "../") {
		return "", false
	}

	if op := firstPyPIOperator(requirement); op >= 0 {
		return strings.TrimSpace(strings.Trim(requirement[op:], "()")), true
	}
	return "", false
}

func firstPyPIOperator(s string) int {
	first := -1
	for _, op := range []string{"===", "~=", "==", "!=", "<=", ">=", "<", ">"} {
		if i := strings.Index(s, op); i >= 0 && (first == -1 || i < first) {
			first = i
		}
	}
	return first
}

func pypiSpecMentionsPrerelease(spec string) bool {
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		op := firstPyPIOperator(part)
		if op < 0 {
			continue
		}
		version := strings.TrimSpace(part[op:])
		for _, prefix := range []string{"===", "~=", "==", "!=", "<=", ">=", "<", ">"} {
			version = strings.TrimPrefix(version, prefix)
		}
		version = strings.TrimSpace(strings.TrimSuffix(version, ".*"))
		if version == "" {
			continue
		}
		v, err := pep440.Parse(version)
		if err == nil && v.IsPreRelease() {
			return true
		}
	}
	return false
}

func getCachedPyPIVersion(s string) (*pep440.Version, bool) {
	if v, ok := pypiVersionCache.Load(s); ok {
		if v == nil {
			return nil, false
		}
		return v.(*pep440.Version), true
	}
	v, err := pep440.Parse(s)
	if err != nil {
		pypiVersionCache.Store(s, nil)
		return nil, false
	}
	pypiVersionCache.Store(s, &v)
	return &v, true
}

func getCachedPyPISpecifier(s string) (*pep440.Specifiers, bool) {
	if v, ok := pypiSpecifierCache.Load(s); ok {
		if v == nil {
			return nil, false
		}
		return v.(*pep440.Specifiers), true
	}
	spec, err := pep440.NewSpecifiers(s)
	if err != nil {
		pypiSpecifierCache.Store(s, nil)
		return nil, false
	}
	pypiSpecifierCache.Store(s, &spec)
	return &spec, true
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
