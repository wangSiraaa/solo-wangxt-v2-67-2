package registry

import (
	"protocompat/internal/compat"
	"protocompat/internal/deps"
	"protocompat/internal/schema"
)

// --- API request/response types (JSON codec over ConnectRPC) ---

type RegisterVersionRequest struct {
	Package     string              `json:"package"`
	Version     string              `json:"version"`
	Files       []schema.SourceFile `json:"files"`
	BaseVersion string              `json:"base_version,omitempty"`
	Samples     []compat.Sample     `json:"samples,omitempty"`
	// Pins optionally fix dependency packages to exact registered
	// versions. When empty, every imported package is pinned to its
	// latest registered version. A pin carrying a digest additionally
	// verifies that the registered version still has that content.
	Pins []deps.Pin `json:"pins,omitempty"`
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
	// Locks are the resolved dependency edges this version was compiled
	// against and stored with.
	Locks []deps.Lock `json:"locks,omitempty"`
	// LockDigest summarizes Locks; it is the stored dependency summary.
	LockDigest string `json:"lock_digest,omitempty"`
}

// DependencySummary is one package's stored dependency information.
type DependencySummary struct {
	Package string      `json:"package"`
	Version string      `json:"version"`
	Locks   []deps.Lock `json:"locks"`
	Digest  string      `json:"digest"`
}

type GetDependenciesRequest struct {
	Package string `json:"package"`
	Version string `json:"version"`
}

type GetDependenciesResponse struct {
	Summary DependencySummary `json:"summary"`
}

// AnalyzeImpactRequest asks for transitive impact of changing one package
// version (base) into candidate. The analysis is content-addressed: the
// same input always returns the same stored result.
type AnalyzeImpactRequest struct {
	Package          string `json:"package"`
	BaseVersion      string `json:"base_version"`
	CandidateVersion string `json:"candidate_version"`
}

type AnalyzeImpactResponse struct {
	// InputDigest identifies the exact snapshot+input the result belongs
	// to. Repeating the request returns the same digest and result.
	InputDigest string         `json:"input_digest"`
	Analysis    ImpactAnalysis `json:"analysis"`
}

// ImpactAnalysis is the JSON-stable shape of impact.Analysis.
type ImpactAnalysis struct {
	Package   string           `json:"package"`
	Base      string           `json:"base_version"`
	Candidate string           `json:"candidate_version"`
	Verdict   compat.Verdict   `json:"verdict"`
	Findings  []compat.Finding `json:"findings"`
	Nodes     []ImpactNode     `json:"nodes"`
}

type ImpactNode struct {
	Package     string             `json:"package"`
	Version     string             `json:"version"`
	Status      string             `json:"status"`
	Verdict     compat.Verdict     `json:"verdict"`
	ReasonPaths []ImpactReasonPath `json:"reason_paths"`
}

type ImpactReasonPath struct {
	Path  []string           `json:"path"`
	Edges []ImpactEdgeReason `json:"edges"`
}

type ImpactEdgeReason struct {
	From    string         `json:"from"`
	To      string         `json:"to"`
	Kind    string         `json:"kind"`
	Verdict compat.Verdict `json:"verdict"`
	Symbols []string       `json:"symbols,omitempty"`
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
