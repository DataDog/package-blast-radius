package blast

import "testing"

func TestNpmMatches(t *testing.T) {
	tests := []struct {
		constraint string
		version    string
		want       bool
	}{
		// Caret ranges
		{"^1.14.0", "1.14.1", true},
		{"^1.14.0", "1.99.0", true},
		{"^1.14.0", "2.0.0", false},
		{"^1.14.0", "1.13.0", false},
		{"^0.0.1", "0.0.1", true},
		{"^0.0.1", "0.0.2", false},

		// Tilde ranges
		{"~1.14.0", "1.14.1", true},
		{"~1.14.0", "1.14.9", true},
		{"~1.14.0", "1.15.0", false},

		// Exact versions
		{"1.14.1", "1.14.1", true},
		{"1.14.0", "1.14.1", false},

		// Comparison operators
		{">=1.0.0", "1.14.1", true},
		{">=2.0.0", "1.14.1", false},
		{">1.14.0", "1.14.1", true},
		{"<2.0.0", "1.14.1", true},
		{"<=1.14.1", "1.14.1", true},

		// OR ranges
		{">=1.0.0 <1.14.0 || >=2.0.0", "1.14.1", false},
		{">=1.0.0 <2.0.0 || >=3.0.0", "1.14.1", true},

		// X-ranges / wildcards
		{"*", "1.14.1", true},
		{"1.x", "1.14.1", true},
		{"1.14.x", "1.14.1", true},
		{"2.x", "1.14.1", false},

		// Hyphen ranges
		{"1.0.0 - 2.0.0", "1.14.1", true},
		{"1.0.0 - 1.14.0", "1.14.1", false},
		{"2.0.0 - 3.0.0", "1.14.1", false},

		// Special strings
		{"", "1.14.1", true},
		{"latest", "1.14.1", true},

		// Non-semver constraints (should return false)
		{"git+https://github.com/foo/bar.git", "1.14.1", false},
		{"https://example.com/pkg.tgz", "1.14.1", false},
		{"file:../my-lib", "1.14.1", false},

		// Space-separated AND, which node-semver uses but many range
		// libraries express with a comma instead.
		{">=1.0.0 <2.0.0", "1.5.0", true},
		{">=1.0.0 <2.0.0", "2.5.0", false},
		{">=1.0.0 <2.0.0", "0.9.0", false},

		// Partial constraints: npm allows omitting minor and patch.
		{"^1", "1.5.0", true},
		{"^1", "2.0.0", false},
		{"^1.2", "1.5.0", true},
		{"~1.2", "1.2.9", true},

		// Prereleases are excluded unless the constraint itself names one,
		// matching npm's rule that they are opt-in.
		{"^1.0.0", "1.5.0-beta.1", false},
		{">=1.0.0", "1.5.0-beta.1", false},
		{"1.2.3-beta.1", "1.2.3-beta.1", true},

		// Build metadata is ignored when comparing.
		{"1.2.3", "1.2.3+build.5", true},

		// Non-semver target versions can't match anything.
		{"^1.0.0", "not-a-version", false},
		{"^1.0.0", "", false},

		// Whitespace around an otherwise valid range.
		{"  ^1.14.0  ", "1.14.1", true},
	}

	for _, tt := range tests {
		t.Run(tt.constraint+"_"+tt.version, func(t *testing.T) {
			got := MatchesVersion(NPM, tt.constraint, tt.version)
			if got != tt.want {
				t.Errorf("MatchesVersion(NPM, %q, %q) = %v, want %v", tt.constraint, tt.version, got, tt.want)
			}
		})
	}
}
