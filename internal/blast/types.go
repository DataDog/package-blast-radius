package blast

import "time"

type PackageVersion struct {
	System  Ecosystem
	Name    string
	Version string
}

func (pv PackageVersion) String() string {
	return pv.Name + "@" + pv.Version
}

// PathStep represents one hop in the dependency chain.
type PathStep struct {
	Package     string // e.g. "tremendous"
	Version     string // e.g. "3.11.0"
	Requirement string // the declared range for the next step, e.g. "^1.6.1"
}

type AffectedPackage struct {
	PackageVersion
	Depth           int
	Path            []PathStep     // from this package down to the target
	Target          PackageVersion // the compromised package this affected pkg traces to
	WeeklyDownloads int64
}

// TargetSpec is a compromised package and the affected versions to check against.
type TargetSpec struct {
	System   Ecosystem
	Name     string
	Versions []string
}

type BlastResult struct {
	Targets        []PackageVersion // expanded: one entry per (name, version) pair
	TotalEdges     int
	Affected       []AffectedPackage
	UniquePackages int
	MaxDepth       int
	Elapsed        time.Duration
}
