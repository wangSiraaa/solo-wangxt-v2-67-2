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
