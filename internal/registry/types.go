package registry

import (
	"protocompat/internal/compat"
	"protocompat/internal/schema"
)

// --- API request/response types (JSON codec over ConnectRPC) ---

type RegisterVersionRequest struct {
	Package     string              `json:"package"`
	Version     string              `json:"version"`
	Files       []schema.SourceFile `json:"files"`
	BaseVersion string              `json:"base_version,omitempty"`
	Samples     []compat.Sample     `json:"samples,omitempty"`
	// RequireCompatible refuses registration when the check against the
	// base version proves an incompatibility (verdict INCOMPATIBLE).
	RequireCompatible bool `json:"require_compatible,omitempty"`
}

type RegisterVersionResponse struct {
	Package        string         `json:"package"`
	Version        string         `json:"version"`
	ContentHash    string         `json:"content_hash"`
	AlreadyExisted bool           `json:"already_existed"`
	BaseVersion    string         `json:"base_version,omitempty"`
	Compatibility  *compat.Report `json:"compatibility,omitempty"`
}

type CheckRequest struct {
	Package          string              `json:"package"`
	BaseVersion      string              `json:"base_version"`
	CandidateVersion string              `json:"candidate_version,omitempty"`
	CandidateFiles   []schema.SourceFile `json:"candidate_files,omitempty"`
	Samples          []compat.Sample     `json:"samples,omitempty"`
	Consumer         string              `json:"consumer,omitempty"`
}

type CheckResponse struct {
	BaseVersion    string          `json:"base_version"`
	HeadVersion    string          `json:"head_version"`
	Report         *compat.Report  `json:"report"`
	ConsumerImpact *ConsumerImpact `json:"consumer_impact,omitempty"`
}

type ConsumerImpact struct {
	Consumer string           `json:"consumer"`
	Encoding string           `json:"encoding"`
	Verdict  compat.Verdict   `json:"verdict"`
	Findings []compat.Finding `json:"findings"`
}

type Usage struct {
	Message string   `json:"message"`          // fully-qualified message name
	Fields  []string `json:"fields,omitempty"` // field paths; empty = whole message
}

type DeclareConsumerRequest struct {
	Package  string  `json:"package"`
	Consumer string  `json:"consumer"`
	Encoding string  `json:"encoding"` // "wire", "json" or "both"
	Usages   []Usage `json:"usages,omitempty"`
}

type DeclareConsumerResponse struct {
	Declared bool `json:"declared"`
}

type ListVersionsRequest struct {
	Package string `json:"package"`
}

type VersionMeta struct {
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
	CreatedAt   string `json:"created_at"`
}

type ListVersionsResponse struct {
	Package  string        `json:"package"`
	Versions []VersionMeta `json:"versions"`
}

// --- cross-package dependency locking & transitive impact analysis ---

// Impact classification of one dependent package version.
const (
	// ImpactDirect: the package directly locked the changed package and
	// the change could not be proven safe for it.
	ImpactDirect = "DIRECT"
	// ImpactTransitive: the package reaches the changed package through
	// at least one intermediate dependency and is affected.
	ImpactTransitive = "TRANSITIVE"
	// ImpactVerifiedUnaffected: the package depends on the changed
	// package, but every finding was verified irrelevant to the symbols
	// it actually references.
	ImpactVerifiedUnaffected = "VERIFIED_UNAFFECTED"
)

// LockedDep is one pinned dependency as reported by the API.
type LockedDep struct {
	Package     string `json:"package"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"` // hex digest captured when the lock was taken
}

// PathHop is one step of a reason path: a package at the version that
// participates in the dependency chain.
type PathHop struct {
	Package string `json:"package"`
	Version string `json:"version"`
}

// ReasonPath is one dependency chain from the changed package to an
// affected package. Diamond dependencies yield several reason paths for
// the same affected package.
type ReasonPath struct {
	Hops []PathHop `json:"hops"`
}

// PackageImpact is the analysis verdict for one dependent package. Each
// dependent appears at most once, however many reason paths lead to it.
type PackageImpact struct {
	Package string `json:"package"`
	// Version is the dependent version that was evaluated (the latest
	// registered version still holding a lock on the chain).
	Version     string           `json:"version"`
	Impact      string           `json:"impact"` // DIRECT | TRANSITIVE | VERIFIED_UNAFFECTED
	ReasonPaths []ReasonPath     `json:"reason_paths"`
	Findings    []compat.Finding `json:"findings,omitempty"`
}

// ImpactResult is the persistable outcome of one analysis.
type ImpactResult struct {
	Report  *compat.Report  `json:"report"`
	Impacts []PackageImpact `json:"impacts"`
}

type AnalyzeImpactRequest struct {
	Package     string `json:"package"`
	BaseVersion string `json:"base_version"`
	HeadVersion string `json:"head_version"`
}

type AnalyzeImpactResponse struct {
	Package     string          `json:"package"`
	BaseVersion string          `json:"base_version"`
	HeadVersion string          `json:"head_version"`
	Report      *compat.Report  `json:"report"`
	Impacts     []PackageImpact `json:"impacts"`
	// Reused is true when the stored analysis for exactly this input was
	// returned instead of recomputing it.
	Reused bool `json:"reused"`
}

type ListDependenciesRequest struct {
	Package string `json:"package"`
	Version string `json:"version"`
}

type ListDependenciesResponse struct {
	Package      string      `json:"package"`
	Version      string      `json:"version"`
	Dependencies []LockedDep `json:"dependencies"`
}
