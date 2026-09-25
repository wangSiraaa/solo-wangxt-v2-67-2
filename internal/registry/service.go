package registry

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	"protocompat/internal/compat"
	"protocompat/internal/schema"
)

// Service implements the registry API. It is pure backend: schema parsing
// goes to protocompile, requests arrive over ConnectRPC, and PostgreSQL
// (via Store) holds packages, versions and consumer declarations.
type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// RegisterVersion compiles the submitted sources, refuses to overwrite an
// existing version with different content, and — when a base version
// exists — attaches an evidence-based compatibility report to the
// response. With require_compatible, a proven-incompatible version is
// rejected before anything is stored.
func (s *Service) RegisterVersion(ctx context.Context, req *connect.Request[RegisterVersionRequest]) (*connect.Response[RegisterVersionResponse], error) {
	r := req.Msg
	if r.Package == "" || r.Version == "" || len(r.Files) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("package, version and files are required"))
	}
	compiled, err := schema.Compile(ctx, r.Files)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Resolve the base version before inserting anything.
	base, baseErr := s.resolveBase(ctx, r.Package, r.Version, r.BaseVersion)

	var report *compat.Report
	if baseErr == nil && base != nil {
		report, err = s.checkAgainst(ctx, base, compiled, r.Samples)
		if err != nil {
			return nil, err
		}
		if r.RequireCompatible && report.Verdict == compat.VerdictIncompatible {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("version %s is proven incompatible with %s (%d FAIL findings); registration refused, nothing stored",
					r.Version, base.Version, countSeverity(report, compat.SeverityFail)))
		}
	}

	created, err := s.store.PutVersion(ctx, Version{
		Package:       r.Package,
		Version:       r.Version,
		ContentHash:   compiled.Hash,
		DescriptorSet: compiled.DescriptorSet,
		OwnedPaths:    compiled.OwnedPaths,
	})
	if errors.Is(err, ErrVersionConflict) {
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("version %s of package %s already exists with different content; versions are immutable and cannot be overwritten", r.Version, r.Package))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if report != nil && created {
		_ = s.store.PutReport(ctx, StoredReport{
			Package: r.Package, BaseVersion: base.Version, HeadVersion: r.Version, Report: report,
		})
	}

	resp := &RegisterVersionResponse{
		Package:        r.Package,
		Version:        r.Version,
		ContentHash:    hex.EncodeToString(compiled.Hash),
		AlreadyExisted: !created,
		Compatibility:  report,
	}
	if base != nil {
		resp.BaseVersion = base.Version
	}
	return connect.NewResponse(resp), nil
}

// CheckCompatibility compares a registered base version against either
// another registered version or inline candidate files. When a consumer
// is named, the report is projected onto that consumer's declared usage.
func (s *Service) CheckCompatibility(ctx context.Context, req *connect.Request[CheckRequest]) (*connect.Response[CheckResponse], error) {
	r := req.Msg
	if r.Package == "" || r.BaseVersion == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("package and base_version are required"))
	}
	if r.CandidateVersion == "" && len(r.CandidateFiles) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("candidate_version or candidate_files is required"))
	}

	base, err := s.store.GetVersion(ctx, r.Package, r.BaseVersion)
	if errors.Is(err, ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("base version %s/%s not found", r.Package, r.BaseVersion))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var headVersion string
	var report *compat.Report
	if r.CandidateVersion != "" {
		head, err := s.store.GetVersion(ctx, r.Package, r.CandidateVersion)
		if errors.Is(err, ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("candidate version %s/%s not found", r.Package, r.CandidateVersion))
		}
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		headVersion = head.Version
		report, err = checkVersions(base, head, r.Samples)
	} else {
		compiled, cerr := schema.Compile(ctx, r.CandidateFiles)
		if cerr != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, cerr)
		}
		headVersion = "(unregistered candidate)"
		report, err = s.checkAgainst(ctx, base, compiled, r.Samples)
	}
	if err != nil {
		return nil, err
	}

	if r.CandidateVersion != "" {
		_ = s.store.PutReport(ctx, StoredReport{
			Package: r.Package, BaseVersion: base.Version, HeadVersion: headVersion, Report: report,
		})
	}

	resp := &CheckResponse{BaseVersion: base.Version, HeadVersion: headVersion, Report: report}
	if r.Consumer != "" {
		impact, err := s.consumerImpact(ctx, r.Package, r.Consumer, report)
		if err != nil {
			return nil, err
		}
		resp.ConsumerImpact = impact
	}
	return connect.NewResponse(resp), nil
}

// DeclareConsumer records which messages/fields a consumer reads and over
// which encoding, so later checks can project findings onto that surface.
func (s *Service) DeclareConsumer(ctx context.Context, req *connect.Request[DeclareConsumerRequest]) (*connect.Response[DeclareConsumerResponse], error) {
	r := req.Msg
	if r.Package == "" || r.Consumer == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("package and consumer are required"))
	}
	switch r.Encoding {
	case "wire", "json", "both":
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("encoding must be wire, json or both, got %q", r.Encoding))
	}
	for _, u := range r.Usages {
		if u.Message == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("usage entries require a message name"))
		}
	}
	if err := s.store.UpsertConsumer(ctx, ConsumerDecl{
		Package: r.Package, Consumer: r.Consumer, Encoding: r.Encoding, Usages: r.Usages,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&DeclareConsumerResponse{Declared: true}), nil
}

// ListVersions returns every registered version of a package.
func (s *Service) ListVersions(ctx context.Context, req *connect.Request[ListVersionsRequest]) (*connect.Response[ListVersionsResponse], error) {
	if req.Msg.Package == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("package is required"))
	}
	versions, err := s.store.ListVersions(ctx, req.Msg.Package)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &ListVersionsResponse{Package: req.Msg.Package}
	for _, v := range versions {
		resp.Versions = append(resp.Versions, VersionMeta{
			Version:     v.Version,
			ContentHash: hex.EncodeToString(v.ContentHash),
			CreatedAt:   v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return connect.NewResponse(resp), nil
}

// --- internals ---

func (s *Service) resolveBase(ctx context.Context, pkg, newVersion, requested string) (*Version, error) {
	if requested != "" {
		base, err := s.store.GetVersion(ctx, pkg, requested)
		if errors.Is(err, ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("base version %s/%s not found", pkg, requested))
		}
		return base, err
	}
	latest, err := s.store.LatestVersion(ctx, pkg)
	if errors.Is(err, ErrNotFound) {
		return nil, nil // first version of the package: nothing to compare
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if latest.Version == newVersion {
		return nil, nil // re-registering the same version; compare is meaningless
	}
	return latest, nil
}

// checkAgainst runs the compat check between a stored base version and a
// freshly compiled candidate.
func (s *Service) checkAgainst(_ context.Context, base *Version, candidate *schema.Compiled, samples []compat.Sample) (*compat.Report, error) {
	oldFiles, err := schema.Load(base.DescriptorSet)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load base descriptors: %w", err))
	}
	owned := unionStrings(base.OwnedPaths, candidate.OwnedPaths)
	return compat.Check(compat.Input{
		Old: oldFiles, New: candidate.Files, OwnedPaths: owned, Samples: samples,
	}), nil
}

func checkVersions(base, head *Version, samples []compat.Sample) (*compat.Report, error) {
	oldFiles, err := schema.Load(base.DescriptorSet)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load base descriptors: %w", err))
	}
	newFiles, err := schema.Load(head.DescriptorSet)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load head descriptors: %w", err))
	}
	owned := unionStrings(base.OwnedPaths, head.OwnedPaths)
	return compat.Check(compat.Input{
		Old: oldFiles, New: newFiles, OwnedPaths: owned, Samples: samples,
	}), nil
}

// consumerImpact projects a report onto a consumer's declared surface:
// encoding dimension plus the messages/fields it actually reads.
func (s *Service) consumerImpact(ctx context.Context, pkg, consumer string, report *compat.Report) (*ConsumerImpact, error) {
	decl, err := s.store.GetConsumer(ctx, pkg, consumer)
	if errors.Is(err, ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("consumer %q has no declaration for package %s", consumer, pkg))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	impact := &ConsumerImpact{Consumer: consumer, Encoding: decl.Encoding, Verdict: compat.VerdictCompatible}
	for _, f := range report.Findings {
		if !dimensionRelevant(decl.Encoding, f.Dimension) {
			continue
		}
		if !usageCovers(decl.Usages, f) {
			continue
		}
		impact.Findings = append(impact.Findings, f)
		switch f.Severity {
		case compat.SeverityFail:
			impact.Verdict = compat.VerdictIncompatible
		case compat.SeverityWarn:
			if impact.Verdict != compat.VerdictIncompatible {
				impact.Verdict = compat.VerdictNeedsReview
			}
		}
	}
	return impact, nil
}

func dimensionRelevant(encoding string, dim compat.Dimension) bool {
	if dim == compat.DimensionBoth || encoding == "both" {
		return true
	}
	if encoding == "wire" {
		return dim == compat.DimensionWire
	}
	return dim == compat.DimensionJSON
}

// usageCovers reports whether a finding touches the declared surface.
// Findings that cannot be attributed to any message (file-level) are
// included conservatively.
func usageCovers(usages []Usage, f compat.Finding) bool {
	if len(usages) == 0 {
		return true // no usage detail: the whole package is in scope
	}
	if f.Message == "" {
		return true
	}
	for _, u := range usages {
		if u.Message != f.Message {
			continue
		}
		if len(u.Fields) == 0 || f.Path == "" {
			return true
		}
		for _, field := range u.Fields {
			if f.Path == field || strings.HasPrefix(f.Path, field+".") || strings.HasPrefix(f.Path, field+"[") {
				return true
			}
		}
	}
	return false
}

func countSeverity(r *compat.Report, sev compat.Severity) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == sev {
			n++
		}
	}
	return n
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
