package viewer

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

//go:embed assets
var embeddedAssets embed.FS

// meta holds the summary fields of a blast radius JSON file, without the
// (potentially gigabyte-sized) affected array.
type meta struct {
	// Target is a human-readable string, and in multi-target runs it is a
	// sentence rather than a package: "2234 versions across 444 packages".
	// Use Targets for anything that needs actual packages.
	Target         string
	Targets        []blast.JSONTarget
	System         string
	TotalEdges     int
	TotalAffected  int
	UniquePackages int
	MaxDepth       int
	Elapsed        string
}

// Options configures the viewer beyond the report itself.
type Options struct {
	Port int
	// AssetsDir serves the UI from disk instead of the embedded copy, so the
	// front end can be edited without rebuilding the binary.
	AssetsDir string
}

// Serve loads a `blast-radius analyze --output json` file and serves the
// exploration UI on 127.0.0.1:port. It blocks until the server exits.
func Serve(jsonPath string, opts Options) error {
	// Resolved before the report, which can take a minute to load: a bad
	// --assets-dir should fail immediately.
	assets, err := assetFS(opts.AssetsDir)
	if err != nil {
		return err
	}

	s, err := loadStore(jsonPath)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	registerRoutes(mux, s, jsonPath, assets)

	addr := fmt.Sprintf("127.0.0.1:%d", opts.Port)
	fmt.Fprintf(os.Stderr, "\nServing at http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

// assetFS roots the UI at the assets directory, from disk when dir is set and
// from the binary otherwise, so both cases see identical request paths.
func assetFS(dir string) (fs.FS, error) {
	if dir != "" {
		if _, err := os.Stat(dir + "/index.html"); err != nil {
			return nil, fmt.Errorf("assets dir %q has no index.html: %w", dir, err)
		}
		return os.DirFS(dir), nil
	}
	return fs.Sub(embeddedAssets, "assets")
}

func registerRoutes(mux *http.ServeMux, s *store, sourcePath string, assets fs.FS) {
	mux.HandleFunc("/api/summary", s.handleSummary(sourcePath))
	mux.HandleFunc("/api/packages", s.handlePackages)
	mux.HandleFunc("/api/package", s.handlePackage)
	mux.HandleFunc("/api/pareto", s.handlePareto)
	mux.HandleFunc("/api/scopes", s.handleScopes)
	mux.HandleFunc("/api/download", s.handleDownload)
	mux.HandleFunc("/api/graph/roots", s.handleGraphRoots)
	mux.HandleFunc("/api/graph/dependents", s.handleGraphDependents)

	mux.Handle("/", noCache(http.FileServerFS(assets)))
}

// noCache keeps a --assets-dir edit one reload away. The server is local and
// short-lived, so there is nothing to gain from caching the UI either way.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func formatPathForCSV(path []blast.JSONStep, target string) string {
	if len(path) == 0 {
		return target
	}
	var b strings.Builder
	for _, s := range path {
		fmt.Fprintf(&b, "%s@%s --(%s)--> ", s.Package, s.Version, s.Requirement)
	}
	b.WriteString(target)
	return b.String()
}

// intParam reads a query parameter, falling back to def when it is absent or
// unparseable. Values below min are clamped rather than replaced by def, so
// offset=0 stays distinguishable from an unset offset.
func intParam(s string, def, min int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	return v
}

func containsLower(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 32
			}
			d := sub[j]
			if d >= 'A' && d <= 'Z' {
				d += 32
			}
			if c != d {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
