package viewer

import (
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

//go:embed viewer.html
var viewerHTML string

// meta holds the summary fields of a blast radius JSON file, without the
// (potentially gigabyte-sized) affected array.
type meta struct {
	Target         string
	System         string
	TotalEdges     int
	TotalAffected  int
	UniquePackages int
	MaxDepth       int
	Elapsed        string
}

// Serve loads a `blast-radius analyze --output json` file and serves the
// exploration UI on 127.0.0.1:port. It blocks until the server exits.
func Serve(jsonPath string, port int) error {
	summary, deduped, depthCounts, err := load(jsonPath)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	registerRoutes(mux, summary, deduped, depthCounts)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Fprintf(os.Stderr, "\nServing at http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

// load stream-decodes the JSON file and reduces it to one entry per package
// name (the highest version), pre-sorted by weekly downloads.
func load(jsonPath string) (meta, []blast.JSONAffected, map[int]int, error) {
	var summary meta

	fmt.Fprintf(os.Stderr, "Loading %s...\n", jsonPath)
	start := time.Now()

	f, err := os.Open(jsonPath)
	if err != nil {
		return summary, nil, nil, fmt.Errorf("cannot open %s: %w", jsonPath, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)

	openTok, err := dec.Token()
	if err != nil || openTok != json.Delim('{') {
		return summary, nil, nil, fmt.Errorf(
			"expected a JSON object at the start of %s; did you forget --output json when generating this file?", jsonPath)
	}

	var allAffected []blast.JSONAffected

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return summary, nil, nil, fmt.Errorf("reading JSON token: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return summary, nil, nil, fmt.Errorf("expected an object key (string), got %v", tok)
		}

		switch key {
		case "target":
			err = dec.Decode(&summary.Target)
		case "system":
			err = dec.Decode(&summary.System)
		case "total_edges":
			err = dec.Decode(&summary.TotalEdges)
		case "total_affected":
			err = dec.Decode(&summary.TotalAffected)
		case "unique_packages":
			err = dec.Decode(&summary.UniquePackages)
		case "max_depth":
			err = dec.Decode(&summary.MaxDepth)
		case "elapsed":
			err = dec.Decode(&summary.Elapsed)
		case "affected":
			allAffected, err = decodeAffected(dec)
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return summary, nil, nil, fmt.Errorf("decoding %q: %w", key, err)
		}
	}

	fmt.Fprintf(os.Stderr, "Loaded %d entries in %s\n", len(allAffected), time.Since(start).Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "Deduplicating...\n")

	bestByName := make(map[string]*blast.JSONAffected, summary.UniquePackages)
	for i := range allAffected {
		a := &allAffected[i]
		if existing, ok := bestByName[a.Name]; !ok || compareVersions(a.Version, existing.Version) > 0 {
			bestByName[a.Name] = a
		}
	}

	deduped := make([]blast.JSONAffected, 0, len(bestByName))
	for _, a := range bestByName {
		deduped = append(deduped, *a)
	}

	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].WeeklyDownloads != deduped[j].WeeklyDownloads {
			return deduped[i].WeeklyDownloads > deduped[j].WeeklyDownloads
		}
		return deduped[i].Name < deduped[j].Name
	})

	depthCounts := make(map[int]int)
	for _, a := range deduped {
		depthCounts[a.Depth]++
	}

	fmt.Fprintf(os.Stderr, "Ready: %d unique packages\n", len(deduped))
	for d := 1; d <= summary.MaxDepth; d++ {
		if c, ok := depthCounts[d]; ok {
			fmt.Fprintf(os.Stderr, "  Depth %d: %d packages\n", d, c)
		}
	}

	return summary, deduped, depthCounts, nil
}

// decodeAffected streams the affected array element by element; these files
// reach ~1 GB, so decoding the whole document at once is not an option.
func decodeAffected(dec *json.Decoder) ([]blast.JSONAffected, error) {
	if _, err := dec.Token(); err != nil { // opening [
		return nil, err
	}

	var out []blast.JSONAffected
	for dec.More() {
		var a blast.JSONAffected
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("entry %d: %w", len(out), err)
		}
		out = append(out, a)
		if len(out)%500000 == 0 {
			fmt.Fprintf(os.Stderr, "  Read %d entries...\n", len(out))
		}
	}

	if _, err := dec.Token(); err != nil { // closing ]
		return nil, err
	}
	return out, nil
}

// filterAndSort applies the query parameters the UI sends to the deduped list.
func filterAndSort(deduped []blast.JSONAffected, q url.Values) []blast.JSONAffected {
	search := q.Get("search")
	minDepth := intParam(q.Get("minDepth"), 1)
	maxDepthFilter := intParam(q.Get("maxDepth"), 99)
	minDl := int64(intParam(q.Get("minDownloads"), 0))
	sortBy := q.Get("sort")
	if sortBy == "" {
		sortBy = "weekly_downloads"
	}
	sortDir := q.Get("dir")
	if sortDir == "" {
		sortDir = "desc"
	}

	filtered := make([]blast.JSONAffected, 0)
	for _, a := range deduped {
		if a.Depth < minDepth || a.Depth > maxDepthFilter {
			continue
		}
		if minDl > 0 && a.WeeklyDownloads < minDl {
			continue
		}
		if search != "" && !matchesSearch(a, search) {
			continue
		}
		filtered = append(filtered, a)
	}

	sort.Slice(filtered, func(i, j int) bool {
		var less bool
		switch sortBy {
		case "name":
			less = filtered[i].Name < filtered[j].Name
		case "depth":
			less = filtered[i].Depth < filtered[j].Depth
		case "version":
			less = filtered[i].Version < filtered[j].Version
		default: // weekly_downloads
			if filtered[i].WeeklyDownloads != filtered[j].WeeklyDownloads {
				less = filtered[i].WeeklyDownloads < filtered[j].WeeklyDownloads
			} else {
				less = filtered[i].Name > filtered[j].Name
			}
		}
		if sortDir == "desc" {
			return !less
		}
		return less
	})

	return filtered
}

func registerRoutes(mux *http.ServeMux, summary meta, deduped []blast.JSONAffected, depthCounts map[int]int) {
	// Filtered, sorted, paginated slice of the deduped list.
	mux.HandleFunc("/api/packages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filtered := filterAndSort(deduped, q)

		limit := intParam(q.Get("limit"), 100)
		offset := intParam(q.Get("offset"), 0)

		total := len(filtered)
		if offset > len(filtered) {
			offset = len(filtered)
		}
		filtered = filtered[offset:]
		if limit > 0 && len(filtered) > limit {
			filtered = filtered[:limit]
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total":   total,
			"offset":  offset,
			"results": filtered,
		})
	})

	// The full filtered+sorted result set, streamed as CSV.
	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		filtered := filterAndSort(deduped, r.URL.Query())

		filename := fmt.Sprintf("blast-radius-%d.csv", time.Now().Unix())
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

		cw := csv.NewWriter(w)
		defer cw.Flush()

		cw.Write([]string{"package", "version", "depth", "weekly_downloads", "target", "path"})
		for _, a := range filtered {
			downloads := ""
			if a.WeeklyDownloads >= 0 {
				downloads = strconv.FormatInt(a.WeeklyDownloads, 10)
			}
			target := a.Target
			if target == "" {
				target = summary.Target
			}
			cw.Write([]string{
				a.Name,
				a.Version,
				strconv.Itoa(a.Depth),
				downloads,
				target,
				formatPathForCSV(a.Path, target),
			})
		}
	})

	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"target":          summary.Target,
			"system":          summary.System,
			"total_edges":     summary.TotalEdges,
			"total_affected":  summary.TotalAffected,
			"unique_packages": summary.UniquePackages,
			"max_depth":       summary.MaxDepth,
			"elapsed":         summary.Elapsed,
			"depth_counts":    depthCounts,
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, viewerHTML)
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

func intParam(s string, def int) int {
	if s == "" {
		return def
	}
	var v int
	fmt.Sscanf(s, "%d", &v)
	if v <= 0 {
		return def
	}
	return v
}

func matchesSearch(a blast.JSONAffected, search string) bool {
	if containsLower(a.Name, search) {
		return true
	}
	for _, s := range a.Path {
		if containsLower(s.Package, search) {
			return true
		}
	}
	return false
}

// compareVersions does a best-effort semver comparison.
// Returns >0 if a > b, <0 if a < b, 0 if equal.
func compareVersions(a, b string) int {
	pa := parseVersion(a)
	pb := parseVersion(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] - pb[i]
		}
	}
	return len(pa) - len(pb)
}

func parseVersion(v string) []int {
	// Strip leading v, split on . and -, take numeric parts
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
