// Package compat compares two fully linked protobuf descriptor closures
// and reports evidence about wire-encoding and JSON-mapping compatibility.
//
// The guiding rule: a difference is only reported as compatible when the
// descriptors prove it. Anything that cannot be proven safe is WARN, which
// keeps the overall verdict at NEEDS_REVIEW instead of COMPATIBLE.
package compat

// Severity ranks a single finding.
type Severity string

const (
	// SeverityFail is proven incompatibility for at least one dimension.
	SeverityFail Severity = "FAIL"
	// SeverityWarn means the descriptors cannot prove the change safe;
	// a human must review it. Never silently treated as pass.
	SeverityWarn Severity = "WARN"
	// SeverityInfo is provably safe context (e.g. a properly reserved
	// field deletion).
	SeverityInfo Severity = "INFO"
)

// Dimension identifies which mapping a finding affects.
type Dimension string

const (
	// DimensionWire affects the binary wire encoding.
	DimensionWire Dimension = "WIRE"
	// DimensionJSON affects the canonical proto JSON mapping.
	DimensionJSON Dimension = "JSON"
	// DimensionBoth affects both encodings.
	DimensionBoth Dimension = "BOTH"
)

// Verdict is the aggregate result of a comparison.
type Verdict string

const (
	// VerdictCompatible: every check passed with evidence.
	VerdictCompatible Verdict = "COMPATIBLE"
	// VerdictNeedsReview: nothing proven broken, but at least one change
	// could not be proven safe.
	VerdictNeedsReview Verdict = "NEEDS_REVIEW"
	// VerdictIncompatible: at least one proven breaking change.
	VerdictIncompatible Verdict = "INCOMPATIBLE"
)

// Finding is one piece of evidence, located at a message and field path.
type Finding struct {
	Code      string    `json:"code"`
	Severity  Severity  `json:"severity"`
	Dimension Dimension `json:"dimension"`
	// Message is the fully-qualified message or enum name the finding
	// belongs to (empty for file-level findings).
	Message string `json:"message,omitempty"`
	// Path is the field path within Message, e.g. "lines[2].amount".
	Path string `json:"path,omitempty"`
	// Detail is a human-readable explanation with the old/new facts.
	Detail string `json:"detail"`
}

// Report is the outcome of comparing an old descriptor set with a new one.
type Report struct {
	Verdict  Verdict        `json:"verdict"`
	Findings []Finding      `json:"findings"`
	Samples  []SampleResult `json:"samples,omitempty"`
}

// Sample is a payload submitted alongside a schema to verify that old and
// new descriptors parse it to the same value.
type Sample struct {
	Name     string `json:"name,omitempty"`
	Message  string `json:"message"`  // fully-qualified message name
	Encoding string `json:"encoding"` // "json" or "wire"
	Data     string `json:"data"`     // JSON text, or base64 for wire
}

// SampleResult reports how one sample fared under both schemas.
type SampleResult struct {
	Name      string `json:"name,omitempty"`
	Message   string `json:"message"`
	Encoding  string `json:"encoding"`
	Status    string `json:"status"` // OK, MISMATCH, PARSE_ERROR, ERROR
	WireEqual bool   `json:"wire_equal"`
	JSONEqual bool   `json:"json_equal"`
	Diffs     []Diff `json:"diffs,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Diff pinpoints one differing value between the old-schema and
// new-schema parse of the same payload.
type Diff struct {
	Path string `json:"path"` // e.g. acme.Msg.lines[2].amount
	Old  string `json:"old"`
	New  string `json:"new"`
}

// verdictOf aggregates findings into a verdict.
func verdictOf(findings []Finding) Verdict {
	v := VerdictCompatible
	for _, f := range findings {
		switch f.Severity {
		case SeverityFail:
			return VerdictIncompatible
		case SeverityWarn:
			v = VerdictNeedsReview
		}
	}
	return v
}
