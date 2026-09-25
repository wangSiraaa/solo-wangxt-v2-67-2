package registry

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"protocompat/internal/compat"
	"protocompat/internal/schema"
)

// maxReasonPaths caps the simple paths kept per dependent so a heavily
// connected graph cannot make the analysis explode. Realistic registries
// never come close.
const maxReasonPaths = 64

// impactInputHash binds a stored analysis to the exact base/head contents
// it was computed from. Versions are immutable, so one (package, base,
// head) triple always maps to one hash — and therefore one result.
func impactInputHash(base, head *Version) []byte {
	h := sha256.New()
	h.Write([]byte("impact-v1"))
	h.Write([]byte{0})
	h.Write(base.ContentHash)
	h.Write([]byte{0})
	h.Write(head.ContentHash)
	return h.Sum(nil)
}

// analyzeImpact compares base->head of pkg and walks the reverse
// dependency graph: every package version holding a lock on pkg is
// evaluated exactly once, however many dependency paths lead to it.
func (s *Service) analyzeImpact(ctx context.Context, pkg string, base, head *Version) (*ImpactResult, error) {
	report, err := checkVersions(base, head, nil)
	if err != nil {
		return nil, err
	}

	// Collect every simple reason path from pkg to its dependents with a
	// worklist: whenever a node gains a path, its dependents are
	// revisited. Registration rejects cycles, so this terminates.
	paths := map[string][]ReasonPath{}
	evalVersion := map[string]string{}
	paths[pkg] = []ReasonPath{{Hops: []PathHop{{Package: pkg, Version: head.Version}}}}
	worklist := []string{pkg}

	for len(worklist) > 0 {
		cur := worklist[0]
		worklist = worklist[1:]
		edges, err := s.store.Dependents(ctx, cur)
		if err != nil {
			return nil, connectErr(err)
		}
		// One evaluated version per dependent package: the latest
		// registered version still holding a lock on cur.
		latest := map[string]DependentEdge{}
		for _, e := range edges {
			if e.Package == cur {
				continue // defensive: self-edges are rejected at registration
			}
			if prev, ok := latest[e.Package]; !ok || versionLess(ctx, s.store, e.Package, prev.Version, e.Version) {
				latest[e.Package] = e
			}
		}
		names := make([]string, 0, len(latest))
		for name := range latest {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			edge := latest[name]
			grew := false
			for _, p := range paths[cur] {
				if pathContains(p, name) {
					continue // simple paths only
				}
				hops := append(append([]PathHop(nil), p.Hops...), PathHop{Package: name, Version: edge.Version})
				np := ReasonPath{Hops: hops}
				if !hasPath(paths[name], np) {
					if len(paths[name]) < maxReasonPaths {
						paths[name] = append(paths[name], np)
					}
					grew = true
				}
			}
			if grew {
				// Evaluate the latest registered version participating in
				// any reason path.
				if prev, ok := evalVersion[name]; !ok || versionLess(ctx, s.store, name, prev, edge.Version) {
					evalVersion[name] = edge.Version
				}
				worklist = append(worklist, name)
			}
		}
	}

	result := &ImpactResult{Report: report}
	dependents := make([]string, 0, len(evalVersion))
	for name := range evalVersion {
		dependents = append(dependents, name)
	}
	sort.Strings(dependents)
	for _, name := range dependents {
		impact, err := s.evalDependent(ctx, pkg, base, head, report, name, evalVersion[name], paths[name])
		if err != nil {
			return nil, err
		}
		result.Impacts = append(result.Impacts, *impact)
	}
	return result, nil
}

// evalDependent determines how the base->head change of pkg affects one
// dependent version. The dependent's own locked world is the evidence:
// the symbols it actually references (through its whole locked closure)
// decide which findings are relevant.
func (s *Service) evalDependent(ctx context.Context, pkg string, base, head *Version, headline *compat.Report, depPkg, depVersion string, reasonPaths []ReasonPath) (*PackageImpact, error) {
	dep, err := s.store.GetVersion(ctx, depPkg, depVersion)
	if err != nil {
		return nil, connectErr(err)
	}

	// Which version of pkg does this dependent's locked world contain?
	// Direct dependents locked it themselves; transitive ones inherit the
	// version their own dependencies were locked against. Registration
	// guarantees a unique answer (diamond conflicts are refused).
	lockedVer, err := s.effectiveDepVersion(ctx, dep, pkg)
	if err != nil {
		return nil, err
	}

	impact := &PackageImpact{
		Package:     depPkg,
		Version:     depVersion,
		ReasonPaths: sortedPaths(reasonPaths),
	}
	depth := minDepth(reasonPaths)

	if lockedVer == "" {
		// No lock path to pkg (defensive; the reverse edge implies one).
		impact.Impact = ImpactVerifiedUnaffected
		return impact, nil
	}
	locked, err := s.store.GetVersion(ctx, pkg, lockedVer)
	if err != nil {
		return nil, connectErr(err)
	}
	if lockedVer == head.Version || locked.CreatedAt.After(head.CreatedAt) {
		// Already at head, or tracking an even newer version registered
		// after head: the transition is already absorbed — nothing to prove.
		impact.Impact = ImpactVerifiedUnaffected
		return impact, nil
	}

	depFiles, err := schema.Load(dep.DescriptorSet)
	if err != nil {
		return nil, connectErr(fmt.Errorf("load dependent descriptors: %w", err))
	}
	headFiles, err := schema.Load(head.DescriptorSet)
	if err != nil {
		return nil, connectErr(fmt.Errorf("load head descriptors: %w", err))
	}

	// Symbols of pkg that the dependent actually reaches through its own
	// files, expanded to their reference closure inside its locked world
	// (which contains pkg's files at the locked version).
	symbols := referencedDepSymbols(depFiles, dep.OwnedPaths, locked.OwnedPaths)

	// The dependent's upgrade question is locked->head, which may differ
	// from the headline base->head when the dependent pinned an older
	// version. Compute it on the locked pair for precise evidence.
	depReport := headline
	if lockedVer != base.Version {
		depReport, err = checkVersions(locked, head, nil)
		if err != nil {
			return nil, err
		}
	}

	var relevant []compat.Finding
	for _, f := range depReport.Findings {
		if f.Message != "" && symbols[f.Message] {
			relevant = append(relevant, f)
		}
	}
	// Symbols the dependent references that head no longer defines are
	// proven breakage — not "cannot prove safe", but proven.
	missing := missingSymbols(symbols, headFiles)
	for _, sym := range missing {
		relevant = append(relevant, compat.Finding{
			Code:      "DEPENDENCY_SYMBOL_REMOVED",
			Severity:  compat.SeverityFail,
			Dimension: compat.DimensionBoth,
			Message:   sym,
			Detail: fmt.Sprintf("%s@%s references %s through its locked dependencies, but %s@%s no longer defines it",
				depPkg, depVersion, sym, pkg, head.Version),
		})
	}

	affected := false
	for _, f := range relevant {
		if f.Severity == compat.SeverityFail || f.Severity == compat.SeverityWarn {
			affected = true
			break
		}
	}
	switch {
	case !affected:
		impact.Impact = ImpactVerifiedUnaffected
	case depth <= 1:
		impact.Impact = ImpactDirect
	default:
		impact.Impact = ImpactTransitive
	}
	impact.Findings = relevant
	return impact, nil
}

// effectiveDepVersion walks the stored lock graph from ver and returns
// the version of targetPkg present in ver's locked world ("" when the
// world does not contain targetPkg).
func (s *Service) effectiveDepVersion(ctx context.Context, ver *Version, targetPkg string) (string, error) {
	type frame struct{ pkg, version string }
	queue := []frame{{ver.Package, ver.Version}}
	visited := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		key := cur.pkg + "\x00" + cur.version
		if visited[key] {
			continue
		}
		visited[key] = true
		if len(visited) > 1024 {
			return "", connectErr(errors.New("dependency closure too deep to resolve"))
		}
		locks, err := s.store.GetDeps(ctx, cur.pkg, cur.version)
		if err != nil {
			return "", connectErr(err)
		}
		for _, l := range locks {
			if l.DepPackage == targetPkg {
				return l.DepVersion, nil
			}
			queue = append(queue, frame{l.DepPackage, l.DepVersion})
		}
	}
	return "", nil
}

// referencedDepSymbols collects the fully-qualified names defined in
// depOwned files that are reachable from the consumer's own files.
// consumerFiles is the consumer's whole locked closure, so reachability
// walks straight through intermediate packages' types: message fields,
// oneof members, map values, extensions and service method types.
func referencedDepSymbols(consumerFiles *protoregistry.Files, consumerOwned, depOwned []string) map[string]bool {
	depFile := map[string]bool{}
	for _, p := range depOwned {
		depFile[p] = true
	}
	owned := map[string]bool{}
	for _, p := range consumerOwned {
		owned[p] = true
	}

	symbols := map[string]bool{}
	seen := map[protoreflect.FullName]bool{}
	var queue []protoreflect.FullName
	visit := func(name protoreflect.FullName) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		queue = append(queue, name)
	}

	// Seed with every type the consumer's own files name.
	consumerFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !owned[fd.Path()] {
			return true
		}
		seedFileRefs(fd, visit)
		return true
	})

	// Expand through the locked world; whatever resolves to a file owned
	// by the changed package is a symbol the consumer depends on.
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		d, err := consumerFiles.FindDescriptorByName(name)
		if err != nil {
			continue
		}
		if depFile[d.ParentFile().Path()] {
			switch d.(type) {
			case protoreflect.MessageDescriptor, protoreflect.EnumDescriptor:
				symbols[string(name)] = true
			}
		}
		if md, ok := d.(protoreflect.MessageDescriptor); ok {
			seedMessageRefs(md, visit)
		}
	}
	return symbols
}

// seedFileRefs feeds every type referenced by the declarations of one
// file into seed.
func seedFileRefs(fd protoreflect.FileDescriptor, seed func(protoreflect.FullName)) {
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		seedMessageRefs(msgs.Get(i), seed)
	}
	svcs := fd.Services()
	for i := 0; i < svcs.Len(); i++ {
		methods := svcs.Get(i).Methods()
		for j := 0; j < methods.Len(); j++ {
			m := methods.Get(j)
			seed(m.Input().FullName())
			seed(m.Output().FullName())
		}
	}
	exts := fd.Extensions()
	for i := 0; i < exts.Len(); i++ {
		seed(exts.Get(i).ContainingMessage().FullName())
	}
}

// seedMessageRefs feeds every type referenced by a message's fields
// (including nested declarations) into seed.
func seedMessageRefs(md protoreflect.MessageDescriptor, seed func(protoreflect.FullName)) {
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.Message() != nil {
			seed(f.Message().FullName())
		}
		if f.Enum() != nil {
			seed(f.Enum().FullName())
		}
		if f.IsExtension() {
			seed(f.ContainingMessage().FullName())
		}
	}
	nested := md.Messages()
	for i := 0; i < nested.Len(); i++ {
		if !nested.Get(i).IsMapEntry() {
			seedMessageRefs(nested.Get(i), seed)
		}
	}
}

// missingSymbols returns the sorted symbols that no longer exist in files.
func missingSymbols(symbols map[string]bool, files *protoregistry.Files) []string {
	var out []string
	for sym := range symbols {
		if _, err := files.FindDescriptorByName(protoreflect.FullName(sym)); err != nil {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

// --- reason path helpers ---

func pathContains(p ReasonPath, pkg string) bool {
	for _, h := range p.Hops {
		if h.Package == pkg {
			return true
		}
	}
	return false
}

func hasPath(paths []ReasonPath, want ReasonPath) bool {
	for _, p := range paths {
		if pathKey(p) == pathKey(want) {
			return true
		}
	}
	return false
}

func pathKey(p ReasonPath) string {
	var b strings.Builder
	for _, h := range p.Hops {
		b.WriteString(h.Package)
		b.WriteByte('@')
		b.WriteString(h.Version)
		b.WriteByte(0)
	}
	return b.String()
}

func sortedPaths(paths []ReasonPath) []ReasonPath {
	out := append([]ReasonPath(nil), paths...)
	sort.Slice(out, func(i, j int) bool { return pathKey(out[i]) < pathKey(out[j]) })
	return out
}

// minDepth is the smallest number of dependency hops from the changed
// package to this dependent (path length minus the root hop).
func minDepth(paths []ReasonPath) int {
	min := -1
	for _, p := range paths {
		d := len(p.Hops) - 1
		if min < 0 || d < min {
			min = d
		}
	}
	return min
}

// versionLess reports whether version a of pkg was registered before
// version b. Falls back to lexical order if a record is missing.
func versionLess(ctx context.Context, store Store, pkg, a, b string) bool {
	if a == b {
		return false
	}
	va, errA := store.GetVersion(ctx, pkg, a)
	vb, errB := store.GetVersion(ctx, pkg, b)
	if errA != nil || errB != nil {
		return a < b
	}
	if va.CreatedAt.Equal(vb.CreatedAt) {
		return a < b
	}
	return va.CreatedAt.Before(vb.CreatedAt)
}
