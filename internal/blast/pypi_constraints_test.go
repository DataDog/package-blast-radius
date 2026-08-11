package blast

import "testing"

func TestPyPIMatches(t *testing.T) {
	tests := []struct {
		requirement string
		version     string
		want        bool
	}{
		// Comparisons and comma-separated AND.
		{">=1.0", "1.5", true},
		{">=1.0,<2", "1.5", true},
		{">=1.0,<2", "2.0", false},
		{"!=1.5,>=1.0,<2", "1.5", false},

		// Compatible releases.
		{"~=1.4.5", "1.4.9", true},
		{"~=1.4.5", "1.5.0", false},
		{"~=1.4", "1.9", true},
		{"~=1.4", "2.0", false},

		// Equality and prefix matching.
		{"==1.2.3", "1.2.3", true},
		{"==1.2.*", "1.2.9", true},
		{"==1.2.*", "1.3.0", false},
		{"==1.2.3", "1.2.3+local.1", true},
		{"==1.2.3+local.1", "1.2.3+local.1", true},
		{"==1.2.3+local.1", "1.2.3+local.2", false},

		// PEP 440 version forms.
		{">=1!2.0", "1!2.1", true},
		{">=1!2.0", "2.1", false},
		{">1.0.post1", "1.0.post2", true},
		{">1.0.dev1", "1.0.dev2", true},

		// Prerelease targets are excluded by default unless the specifier names a
		// prerelease/dev release.
		{">=1.0", "1.1a1", false},
		{">=1.1a1", "1.1b1", true},
		{"<2.0", "1.9a1", false},
		{"<2.0a1", "1.9a1", true},

		// deps.dev can preserve PEP 508 context around the version specifier.
		{">=2.0 ; python_version >= '3.11'", "2.1", true},
		{"requests>=2.0,<3 ; extra == 'socks'", "2.31.0", true},
		{"requests (>=2.0,<3)", "2.31.0", true},
		{"(>=1.0,<2.0)", "1.5", true},
		{"", "9999", true},

		// Non-index requirements and malformed inputs do not match public
		// compromised package versions.
		{"pkg @ https://example.com/pkg.whl", "1.0", false},
		{"https://example.com/pkg.whl", "1.0", false},
		{"git+https://example.com/pkg.git", "1.0", false},
		{"file:../pkg", "1.0", false},
		{">=1.0", "not-a-version", false},
		{"not a specifier", "1.0", false},
	}

	for _, tt := range tests {
		t.Run(tt.requirement+"_"+tt.version, func(t *testing.T) {
			got := MatchesVersion(PyPI, tt.requirement, tt.version)
			if got != tt.want {
				t.Errorf("MatchesVersion(PyPI, %q, %q) = %v, want %v", tt.requirement, tt.version, got, tt.want)
			}
		})
	}
}

func TestNormalizePyPIName(t *testing.T) {
	tests := map[string]string{
		"Requests":           "requests",
		"my.package_name":    "my-package-name",
		"  A---B.__C  ":      "a-b-c",
		"already-normalized": "already-normalized",
	}
	for in, want := range tests {
		if got := NormalizePackageName(PyPI, in); got != want {
			t.Errorf("NormalizePackageName(PyPI, %q) = %q, want %q", in, got, want)
		}
	}
}
