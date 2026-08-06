package blast

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want string // "gt", "lt", or "eq"
	}{
		{"2.0.0", "1.0.0", "gt"},
		{"1.0.0", "2.0.0", "lt"},
		{"1.0.0", "1.0.0", "eq"},
		{"1.10.0", "1.9.0", "gt"}, // numeric, not lexicographic
		{"1.0.10", "1.0.9", "gt"},
		{"v2.0.0", "1.0.0", "gt"}, // semver tolerates the leading v

		// Prereleases order below their own release, which the numeric-prefix
		// comparison this replaced got wrong.
		{"1.0.0-beta", "1.0.0", "lt"},
		{"1.0.0-beta.2", "1.0.0-beta.10", "lt"},
		{"1.0.0-alpha", "1.0.0-beta", "lt"},

		// Registry data is full of strings semver rejects. They compare by
		// numeric segments among themselves and sort below anything valid.
		{"1.2.3.4", "1.2.3.5", "lt"},
		{"not-a-version", "1.0.0", "lt"},
		{"1.0.0", "not-a-version", "gt"},
		{"junk", "junk", "eq"},
	}

	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		var label string
		switch {
		case got > 0:
			label = "gt"
		case got < 0:
			label = "lt"
		default:
			label = "eq"
		}
		if label != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d (%s), want %s", tt.a, tt.b, got, label, tt.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	v, ok := ParseVersion("1.2.3-rc.1")
	if !ok {
		t.Fatal("ParseVersion(1.2.3-rc.1) failed")
	}
	if v.Major() != 1 || v.Minor() != 2 || v.Patch() != 3 || v.Prerelease() != "rc.1" {
		t.Errorf("parsed as %s", v)
	}

	if _, ok := ParseVersion("not-a-version"); ok {
		t.Error("ParseVersion accepted a non-version")
	}
}
