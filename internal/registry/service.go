package registry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"

	"protocompat/internal/compat"
	"protocompat/internal/deps"
	"protocompat/internal/impact"
	"protocompat/internal/schema"
)

// Service implements the registry API. It is pure backend: schema parsing
// goes to protocompile, requests arrive over ConnectRPC, and PostgreSQL
// (via Store) holds packages, versions, dependency locks, consumer
// declarations and content-addressed impact analyses.
type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// RegisterVersion compiles the submitted sources against immutable,
// pinned dependency versions, refuses to overwrite an existing version
// with different content, and — when a base version exists — attaches an
// evidence-based compatibility report to the response. Missing
// dependencies, dependency cycles and digest mismatches are refused
// before anything is stored; with require_compatible, a proven
// incompatible version is refused as well.
func (s *Service) RegisterVersion(ctx context.Context, req *connect.Request[RegisterVersionRequest]) (*connect.Response[RegisterVersionResponse], error) {
	r := req.Msg
	if r.Package == "" || r.Version == "" || len(r.Files) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("package, version and files are required"))
	}

	// 1. Parse imports without resolving them, so missing dependencies can
	// be reported precisely instead of as a compiler failure deep in the
	// tree.
	importsByFile, err := deps.ExtractImports(r.Files)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	submitted := map[string]bool{}
	for _, f := range r.Files {
		submitted[f.Path] = true
	}

	// 2. Resolve every external import to a registered package version.
	resolved, err := s.resolveDependencies(ctx, r.Package, r.Pins, importsByFile, submitted)
	if err != nil {
		return nil, err
	}

	// 3. Build the path -> pinned descriptor index, rejecting pins that
	// disagree on a shared file's content.
	owners, err := deps.BuildOwnerPaths(resolved)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	// 4. Compile against exactly the pinned closures.
	compiled, err := deps.CompilePinned(ctx, r.Files, owners)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	contentHash, err := schema.OwnedOnlyHash(compiled.DescriptorSet, compiled.OwnedPaths)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	locks := deps.FromLocked(resolved)
	lockDigest := deps.Digest(locks)

	// 5. Cycle pre-check against the package graph of stored versions.
	all, err := s.store.AllVersions(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	graph := packageGraph(all)
	if cycle := graph.AddCycle(r.Package, locks); cycle != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("registration refused, nothing stored: %w", &deps.CycleError{Cycle: cycle}))
	}

	// 6. Resolve the base version before inserting anything.
	base, baseErr := s.resolveBase(ctx, r.Package, r.Version, r.BaseVersion)

	var report *compat.Report
	if baseErr == nil && base != nil {
		report, err = s.checkCompiledAgainst(base, compiled, r.Samples)
		if err != nil {
			return nil, err
		}
		if r.RequireCompatible && report.Verdict == compat.VerdictIncompatible {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("version %s is proven incompatible with %s (%d FAIL findings); registration refused, nothing stored",
					r.Version, base.Version, countSeverity(report, compat.SeverityFail)))
		}
	}

	// 7. Atomically store version, claimed paths and dependency edges.
	created, err := s.store.PutVersion(ctx, Version{
		Package:       r.Package,
		Version:       r.Version,
		ContentHash:   contentHash,
		DescriptorSet: compiled.DescriptorSet,
		OwnedPaths:    compiled.OwnedPaths,
		Locks:         locks,
		LockDigest:    lockDigest,
	})
	switch {
	case errors.Is(err, ErrVersionConflict):
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("version %s of package %s already exists with different content; versions are immutable and cannot be overwritten", r.Version, r.Package))
	case errors.Is(err, ErrDependencyCycle):
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("registration refused, nothing stored: %w", err))
	case err != nil:
		if errors.Is(err, ErrPathConflict) {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("registration refused, nothing stored: %w", err))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if report != nil && created {
		_ = s.store.PutReport(ctx, StoredReport{
			Package: r.Package, BaseVersion: base.Version, HeadVersion: r.Version, Report: report,
		})
	}

	// Idempotent re-registration: echo the lock summary actually stored.
	// Explicit pins must still agree with that immutable history; a
	// differing explicit pin is refused even when content is identical.
	if !created {
		stored, gerr := s.store.GetVersion(ctx, r.Package, r.Version)
		if gerr == nil {
			locks, lockDigest = stored.Locks, stored.LockDigest
			if cerr := checkExplicitPinsAgainst(stored.Locks, r.Pins); cerr != nil {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("version %s of package %s already exists; %w; dependency history is immutable",
						r.Version, r.Package, cerr))
			}
		}
	}

	resp := &RegisterVersionResponse{
		Package:        r.Package,
		Version:        r.Version,
		ContentHash:    hex.EncodeToString(contentHash),
		AlreadyExisted: !created,
		Compatibility:  report,
		Locks:          locks,
		LockDigest:     lockDigest,
	}
	if base != nil {
		resp.BaseVersion = base.Version
	}
	return connect.NewResponse(resp), nil
}

// checkExplicitPinsAgainst verifies that every explicit pin agrees with
// the locks stored on an already-existing version. A pin for a package
// the version never locked, a different pinned version, or a mismatched
// digest, all violate immutability.
func checkExplicitPinsAgainst(stored []deps.Lock, pins []deps.Pin) error {
	if len(pins) == 0 {
		return nil // pure idempotent retry without a lockfile
	}
	byPkg := map[string]deps.Lock{}
	for _, l := range stored {
		byPkg[l.Package] = l
	}
	for _, p := range pins {
		got, ok := byPkg[p.Package]
		if !ok {
			return fmt.Errorf("pin for %s does not match its stored dependency set (that package was not locked)", p.Package)
		}
		if got.Version != p.Version {
			return fmt.Errorf("pin for %s demands %s but the version is locked at %s", p.Package, p.Version, got.Version)
		}
		if p.Digest != "" && p.Digest != got.Digest {
			return fmt.Errorf("pin for %s@%s demands digest %s but the locked digest is %s",
				p.Package, p.Version, p.Digest, got.Digest)
		}
	}
	return nil
}

// resolveDependencies maps each external import to an immutable
// registered package version, honoring explicit pins and defaulting to
// the newest registered version. A missing package or version, a stale
// pin digest, a pin naming a package that is not imported, or an
// imported path no longer present under the pinned version are errors.
func (s *Service) resolveDependencies(ctx context.Context, pkg string, pins []deps.Pin,
	importsByFile map[string][]string, submitted map[string]bool) ([]deps.ResolvedDependency, error) {

	pinByPkg := map[string]deps.Pin{}
	for _, p := range pins {
		if p.Package == "" || p.Version == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("pins require package and version"))
		}
		if p.Package == pkg {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("package %s cannot pin itself", p.Package))
		}
		if _, dup := pinByPkg[p.Package]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("duplicate pin for package %s", p.Package))
		}
		pinByPkg[p.Package] = p
	}

	type wanted struct {
		pkg   string
		paths map[string]bool
	}
	wantByPkg := map[string]*wanted{}
	var missing []string
	for _, paths := range importsByFile {
		for _, path := range paths {
			if !deps.IsExternalImport(path, submitted) {
				continue
			}
			owner, latest, err := s.store.PathOwner(ctx, path)
			if errors.Is(err, ErrNotFound) {
				missing = append(missing, path)
				continue
			}
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if owner == pkg {
				// A path the package itself owns in a previous version
				// but does not resubmit: unsupported self-import.
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("import %q belongs to this package's earlier version; resubmit the file instead of importing it", path))
			}
			w, ok := wantByPkg[owner]
			if !ok {
				w = &wanted{pkg: owner, paths: map[string]bool{}}
				wantByPkg[owner] = w
			}
			w.paths[path] = true
			_ = latest
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"registration refused, nothing stored: %d imported path(s) are not provided by any registered package (register the dependency package first or submit the files): %s",
			len(missing), strings.Join(missing, ", ")))
	}

	var pinnedPkgs []string
	for p := range pinByPkg {
		pinnedPkgs = append(pinnedPkgs, p)
	}
	for _, p := range pinnedPkgs {
		if _, ok := wantByPkg[p]; !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("pin for %s is unused: no submitted file imports that package", p))
		}
	}

	pkgs := make([]string, 0, len(wantByPkg))
	for p := range wantByPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	resolved := make([]deps.ResolvedDependency, 0, len(pkgs))
	for _, p := range pkgs {
		w := wantByPkg[p]
		var v *Version
		if pin, ok := pinByPkg[p]; ok {
			got, err := s.store.GetVersion(ctx, p, pin.Version)
			if errors.Is(err, ErrNotFound) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"registration refused, nothing stored: pinned dependency %s@%s is not registered", p, pin.Version))
			}
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			if pin.Digest != "" && pin.Digest != hex.EncodeToString(got.ContentHash) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"registration refused, nothing stored: dependency summary mismatch for %s@%s: registered digest %s, pin demanded %s",
					p, pin.Version, hex.EncodeToString(got.ContentHash), pin.Digest))
			}
			v = got
		} else {
			latest, err := s.store.LatestVersion(ctx, p)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			v = latest
		}
		owned := map[string]bool{}
		for _, op := range v.OwnedPaths {
			owned[op] = true
		}
		var imported []string
		for path := range w.paths {
			if !owned[path] {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"registration refused, nothing stored: pinned %s@%s does not provide imported path %q", p, v.Version, path))
			}
			imported = append(imported, path)
		}
		sort.Strings(imported)
		resolved = append(resolved, deps.ResolvedDependency{
			Package:       p,
			Version:       v.Version,
			Digest:        v.ContentHash,
			Closure:       v.DescriptorSet,
			OwnedPaths:    v.OwnedPaths,
			ImportedPaths: imported,
		})
	}
	return resolved, nil
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
		report, err = s.checkCompiledAgainst(base, compiled, r.Samples)
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

// AnalyzeImpact computes this package's findings and, from the immutable
// lock history, every directly impacted, transitively impacted and
// verified-unaffected dependent version. Results are memoized by an
// input digest over the exact registry snapshot: repeating the same
// analysis returns the same stored result, while historical results stay
// reproducible after newer versions are registered.
func (s *Service) AnalyzeImpact(ctx context.Context, req *connect.Request[AnalyzeImpactRequest]) (*connect.Response[AnalyzeImpactResponse], error) {
	r := req.Msg
	if r.Package == "" || r.BaseVersion == "" || r.CandidateVersion == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("package, base_version and candidate_version are required"))
	}
	base, err := s.store.GetVersion(ctx, r.Package, r.BaseVersion)
	if errors.Is(err, ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("base version %s/%s not found", r.Package, r.BaseVersion))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	candidate, err := s.store.GetVersion(ctx, r.Package, r.CandidateVersion)
	if errors.Is(err, ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("candidate version %s/%s not found", r.Package, r.CandidateVersion))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	all, err := s.store.AllVersions(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	snap, err := buildSnapshot(all)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	digest := snap.InputDigest(r.Package, r.BaseVersion, r.CandidateVersion)

	if stored, err := s.store.GetImpact(ctx, digest); err == nil {
		var an ImpactAnalysis
		if jerr := json.Unmarshal(stored.Analysis, &an); jerr == nil {
			return connect.NewResponse(&AnalyzeImpactResponse{InputDigest: digest, Analysis: an}), nil
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	result := impact.Analyze(snap, r.Package, base.Version, candidate.Version, func() *compat.Report {
		rep, cerr := checkVersions(base, candidate, nil)
		if cerr != nil {
			return &compat.Report{Verdict: compat.VerdictNeedsReview}
		}
		return rep
	})

	an := convertAnalysis(result)
	body, err := json.Marshal(an)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.store.PutImpact(ctx, StoredImpact{
		InputDigest: digest, Package: r.Package, BaseVersion: r.BaseVersion, Analysis: body,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&AnalyzeImpactResponse{InputDigest: digest, Analysis: an}), nil
}

// GetDependencies returns the stored dependency summary of a version.
func (s *Service) GetDependencies(ctx context.Context, req *connect.Request[GetDependenciesRequest]) (*connect.Response[GetDependenciesResponse], error) {
	r := req.Msg
	if r.Package == "" || r.Version == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("package and version are required"))
	}
	v, err := s.store.GetVersion(ctx, r.Package, r.Version)
	if errors.Is(err, ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("version %s/%s not found", r.Package, r.Version))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if v.Locks == nil {
		v.Locks = []deps.Lock{}
	}
	return connect.NewResponse(&GetDependenciesResponse{Summary: DependencySummary{
		Package: v.Package, Version: v.Version, Locks: v.Locks, Digest: v.LockDigest,
	}}), nil
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

// checkCompiledAgainst runs the compat check between a stored base
// version and a freshly compiled candidate. Only files owned by either
// side are diffed; dependency files in the candidate closure are skipped.
func (s *Service) checkCompiledAgainst(base *Version, candidate *schema.Compiled, samples []compat.Sample) (*compat.Report, error) {
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

// buildSnapshot turns stored versions into the immutable analysis input,
// linking every descriptor closure exactly as it was registered.
func buildSnapshot(all []Version) (impact.Snapshot, error) {
	snap := impact.Snapshot{Versions: map[impact.NodeKey]impact.VersionData{}}
	for _, v := range all {
		files, err := schema.Load(v.DescriptorSet)
		if err != nil {
			return snap, fmt.Errorf("load %s@%s descriptors: %w", v.Package, v.Version, err)
		}
		locks := make([]impact.Lock, 0, len(v.Locks))
		for _, l := range v.Locks {
			locks = append(locks, impact.Lock{Package: l.Package, Version: l.Version})
		}
		snap.Versions[impact.NodeKey{Package: v.Package, Version: v.Version}] = impact.VersionData{
			ContentHash: v.ContentHash,
			Files:       files,
			OwnedPaths:  v.OwnedPaths,
			Locks:       locks,
		}
	}
	return snap, nil
}

func convertAnalysis(a *impact.Analysis) ImpactAnalysis {
	out := ImpactAnalysis{
		Package:   a.Package,
		Base:      a.Base,
		Candidate: a.Candidate,
		Verdict:   a.Verdict,
		Findings:  a.Findings,
	}
	if out.Findings == nil {
		out.Findings = []compat.Finding{}
	}
	for _, n := range a.Nodes {
		node := ImpactNode{
			Package: n.Package, Version: n.Version,
			Status: string(n.Status), Verdict: n.Verdict,
		}
		for _, rp := range n.ReasonPaths {
			p := ImpactReasonPath{Path: rp.Path}
			for _, e := range rp.Edges {
				p.Edges = append(p.Edges, ImpactEdgeReason{
					From: e.From, To: e.To, Kind: e.Kind, Verdict: e.Verdict, Symbols: e.Symbols,
				})
			}
			node.ReasonPaths = append(node.ReasonPaths, p)
		}
		out.Nodes = append(out.Nodes, node)
	}
	if out.Nodes == nil {
		out.Nodes = []ImpactNode{}
	}
	return out
}

// packageGraph builds the package-level dependency graph from stored
// versions, used for the registration-time cycle pre-check.
func packageGraph(all []Version) deps.Graph {
	edges := map[string]map[string]bool{}
	for _, v := range all {
		if edges[v.Package] == nil {
			edges[v.Package] = map[string]bool{}
		}
		for _, l := range v.Locks {
			edges[v.Package][l.Package] = true
		}
	}
	g := deps.Graph{}
	for pkg, set := range edges {
		for dep := range set {
			g[pkg] = append(g[pkg], dep)
		}
	}
	return g
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
