// Package impact computes transitive compatibility impact across locked
// package versions. Given a package and two of its versions, it first
// runs the local compatibility check (this package's findings), then
// walks the reverse dependency graph stored with every version's locks
// and classifies each reachable dependent:
//
//   - DIRECT_IMPACTED: a version locked directly against the changed
//     package's base version whose own schema references the changed
//     surface.
//   - TRANSITIVE_IMPACTED: reached only through another impacted version.
//   - VERIFIED_UNAFFECTED: reverse-reachable (it does lock the base
//     version somewhere in its closure) but its referenced surface and
//     every intermediate surface were proven to avoid the change.
//
// Each impacted node carries every distinct reason path (so diamond
// dependencies keep all causes) while the node itself is reported once.
package impact

import (
	"sort"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"protocompat/internal/compat"
)

// NodeKey identifies one immutable package version.
type NodeKey struct {
	Package string
	Version string
}

func (k NodeKey) String() string { return k.Package + "@" + k.Version }

// Snapshot is the immutable registry view an analysis runs against. It
// deliberately contains no timestamps: analysis results are a pure
// function of descriptor content and locks.
type Snapshot struct {
	// Versions maps every package version to its stored data.
	Versions map[NodeKey]VersionData
}

// VersionData is everything impact analysis needs from one version.
type VersionData struct {
	// ContentHash is the registered package-content hash; it participates
	// in the input digest but never in the analysis itself.
	ContentHash []byte
	// Files is the linked descriptor closure as registered.
	Files *protoregistry.Files
	// OwnedPaths limits "what this package itself defines" to its own
	// files (everything else in the closure is a locked dependency).
	OwnedPaths []string
	// Locks are the dependency edges recorded at registration.
	Locks []Lock
}

// Lock mirrors deps.Lock without importing it (snapshot data may come
// from storage, which has no compiler dependency).
type Lock struct {
	Package string
	Version string
}

// Analysis is the full result for one (package, base, candidate) input.
type Analysis struct {
	Package   string
	Base      string
	Candidate string
	// Verdict is this package's own verdict.
	Verdict  compat.Verdict
	Findings []compat.Finding
	// Nodes holds every reported dependent exactly once, ordered by
	// package then version.
	Nodes []Node
	// ChangedPackage is always the analyzed package; recorded for
	// serialization convenience.
}

// Status classifies a dependent node.
type Status string

const (
	StatusDirect     Status = "DIRECT_IMPACTED"
	StatusTransitive Status = "TRANSITIVE_IMPACTED"
	StatusUnaffected Status = "VERIFIED_UNAFFECTED"
)

// Node is one de-duplicated affected package version.
type Node struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Status  Status `json:"status"`
	// Verdict is what propagates to this node: INCOMPATIBLE when any
	// reason path carries a FAIL finding, NEEDS_REVIEW otherwise.
	Verdict compat.Verdict `json:"verdict"`
	// ReasonPaths are the distinct dependency chains through which impact
	// arrives (or which were checked for verified-unaffected nodes). Each
	// path runs from this node down to the changed package; the edge
	// annotations name the edge's verdict.
	ReasonPaths []ReasonPath `json:"reason_paths"`
}

// ReasonPath is one dependency chain: node[i] locks node[i+1].
type ReasonPath struct {
	Nodes []NodeKey `json:"-"`
	Path  []string  `json:"path"`
	// Edges[i] describes the edge from Path[i] to Path[i+1]: DIRECT for
	// the final edge at the changed package when that edge consumes the
	// change, TRANSIT for edges inside the chain.
	Edges []EdgeReason `json:"edges"`
}

// EdgeReason says why one edge of a reason path propagates impact.
type EdgeReason struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind is DIRECT (the consumer references changed surface) or
	// TRANSIT (impact arrives from a dependency it references).
	Kind string `json:"kind"`
	// Verdict is the strongest finding consumed on this edge:
	// INCOMPATIBLE or NEEDS_REVIEW.
	Verdict compat.Verdict `json:"verdict"`
	// Symbols are the referenced fully-qualified names that carry the
	// change (bounded list, for evidence).
	Symbols []string `json:"symbols,omitempty"`
}

// maxReasonPaths caps how many distinct paths are retained per node; the
// set stays representative and bounded for dense graphs.
const maxReasonPaths = 8

// Analyze runs the local comparison and propagates its effect through the
// reverse lock graph. The local report is computed with the supplied
// checker so this package does not depend on storage semantics.
func Analyze(snap Snapshot, pkg, baseVersion, candidateVersion string,
	check func() *compat.Report) *Analysis {

	rep := check()
	a := &Analysis{
		Package:   pkg,
		Base:      baseVersion,
		Candidate: candidateVersion,
		Verdict:   rep.Verdict,
		Findings:  rep.Findings,
	}

	base := NodeKey{Package: pkg, Version: baseVersion}
	if _, ok := snap.Versions[base]; !ok {
		return a // nothing to walk; local report is still returned
	}

	// 1. Changed surface: FQNs directly named by findings plus the owning
	// messages of every changed field.
	changed := changedSurface(rep.Findings)

	// 2. Reverse graph restricted to edges whose locked version equals
	// the analyzed base version at the root. Dependents pinned to other
	// versions do not consume this change.
	rev := reverseEdges(snap, base)

	// 3. Reverse-reachable set from base (nodes that somehow consume the
	// base version). Needed so reachable-but-clean nodes can be reported
	// as VERIFIED_UNAFFECTED.
	reachable := reachableFrom(base, rev)

	// 4. Taint propagation, layer by layer from the changed package.
	// taint[node] is the set of exposed FQNs that node presents to its
	// own consumers when impact reaches it.
	taint := map[NodeKey]map[string]bool{}
	statusOf := map[NodeKey]Status{}
	verdictOf := map[NodeKey]compat.Verdict{}
	directSyms := map[NodeKey][]string{}

	if len(changed) > 0 {
		var frontier []NodeKey
		for _, dep := range rev[base] {
			consumed, verdict := consumesChange(snap, dep, base, changed, rep)
			if len(consumed) == 0 {
				continue
			}
			frontier = append(frontier, dep)
			statusOf[dep] = StatusDirect
			verdictOf[dep] = strongerVerdict(verdictOf[dep], verdict)
			directSyms[dep] = unionStrings(directSyms[dep], consumed)
			taint[dep] = exposedThrough(snap, dep, consumed)
		}

		// Fixpoint: propagate through reverse layers in DAG order.
		for progress := true; progress; {
			progress = false
			for _, impactedNode := range cloneKeys(taint) {
				for _, upper := range rev[impactedNode] {
					if upper == impactedNode {
						continue
					}
					syms := referencesAny(snap, upper, taint[impactedNode])
					if len(syms) == 0 {
						continue
					}
					if _, ok := taint[upper]; !ok {
						progress = true
					}
					if statusOf[upper] != StatusDirect {
						statusOf[upper] = StatusTransitive
					}
					verdictOf[upper] = strongerVerdict(verdictOf[upper], verdictOf[impactedNode])
					exposed := exposedThrough(snap, upper, syms)
					if taint[upper] == nil {
						taint[upper] = map[string]bool{}
					}
					for s := range exposed {
						if !taint[upper][s] {
							taint[upper][s] = true
							progress = true
						}
					}
				}
			}
		}
	}

	// 5. Collect reason paths and build de-duplicated nodes.
	keys := make([]NodeKey, 0, len(reachable))
	for k := range reachable {
		if k == base {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Package != keys[j].Package {
			return keys[i].Package < keys[j].Package
		}
		return keys[i].Version < keys[j].Version
	})
	for _, k := range keys {
		st, ok := statusOf[k]
		if !ok {
			st = StatusUnaffected
		}
		node := Node{Package: k.Package, Version: k.Version, Status: st}
		if st == StatusUnaffected {
			node.Verdict = compat.VerdictCompatible
			node.ReasonPaths = verifiedPaths(snap, rev, k, base)
		} else {
			node.Verdict = verdictOf[k]
			if node.Verdict == "" {
				node.Verdict = compat.VerdictNeedsReview
			}
			node.ReasonPaths = impactedPaths(snap, rev, k, base, directSyms, taint, statusOf, verdictOf)
		}
		a.Nodes = append(a.Nodes, node)
	}
	return a
}

// changedSurface expands findings into the set of FQNs that count as
// "changed". Field-level findings taint the owning message; file-level
// findings without a message taint nothing (callers cannot reference
// them), and the local verdict still stands on its own.
func changedSurface(findings []compat.Finding) map[string]bool {
	out := map[string]bool{}
	for _, f := range findings {
		if f.Severity == compat.SeverityInfo {
			continue
		}
		if f.Message == "" {
			continue
		}
		out[f.Message] = true
	}
	return out
}

// reverseEdges builds reverse adjacency over the whole snapshot, but only
// including edges whose locked endpoint exists. At the root, edges are
// restricted to versions equal to base (a pin on a different version
// does not consume this comparison's change).
func reverseEdges(snap Snapshot, base NodeKey) map[NodeKey][]NodeKey {
	rev := map[NodeKey]map[NodeKey]bool{}
	for nk, vd := range snap.Versions {
		for _, l := range vd.Locks {
			to := NodeKey{Package: l.Package, Version: l.Version}
			if _, ok := snap.Versions[to]; !ok {
				continue
			}
			if to.Package == base.Package && to.Version != base.Version {
				continue // pinned elsewhere: not a consumer of this change
			}
			if rev[to] == nil {
				rev[to] = map[NodeKey]bool{}
			}
			rev[to][nk] = true
		}
	}
	out := make(map[NodeKey][]NodeKey, len(rev))
	for k, set := range rev {
		for n := range set {
			out[k] = append(out[k], n)
		}
		sort.Slice(out[k], func(i, j int) bool {
			if out[k][i].Package != out[k][j].Package {
				return out[k][i].Package < out[k][j].Package
			}
			return out[k][i].Version < out[k][j].Version
		})
	}
	return out
}

// reachableFrom returns all nodes reachable by reverse edges from root,
// including root.
func reachableFrom(root NodeKey, rev map[NodeKey][]NodeKey) map[NodeKey]bool {
	seen := map[NodeKey]bool{root: true}
	var walk func(NodeKey)
	walk = func(n NodeKey) {
		for _, up := range rev[n] {
			if !seen[up] {
				seen[up] = true
				walk(up)
			}
		}
	}
	walk(root)
	return seen
}

// consumesChange reports which changed FQNs node references directly in
// files it owns, and the strongest verdict carried by those findings.
func consumesChange(snap Snapshot, node, base NodeKey, changed map[string]bool, rep *compat.Report) ([]string, compat.Verdict) {
	refs := referencedFQNs(snap, node)
	var hit []string
	verdict := compat.VerdictCompatible
	for sym := range changed {
		if refs[sym] {
			hit = append(hit, sym)
			verdict = findingsVerdict(rep.Findings, sym)
		}
	}
	sort.Strings(hit)
	if len(hit) > 0 {
		return bounded(hit), verdict
	}
	return nil, verdict
}

// findingsVerdict is the strongest verdict among non-INFO findings
// attributed to the given FQN.
func findingsVerdict(findings []compat.Finding, fqn string) compat.Verdict {
	v := compat.VerdictCompatible
	for _, f := range findings {
		if f.Message != fqn {
			continue
		}
		switch f.Severity {
		case compat.SeverityFail:
			return compat.VerdictIncompatible
		case compat.SeverityWarn:
			if v != compat.VerdictIncompatible {
				v = compat.VerdictNeedsReview
			}
		}
	}
	return v
}

func strongerVerdict(a, b compat.Verdict) compat.Verdict {
	rank := map[compat.Verdict]int{
		compat.VerdictCompatible:   0,
		compat.VerdictNeedsReview:  1,
		compat.VerdictIncompatible: 2,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// referencedFQNs collects every fully-qualified message/enum type name
// referenced from fields in node's owned files (including map
// key/value and oneof fields). Service method types and file options are
// intentionally excluded: API change impact through services is not
// claimed by this analyzer.
func referencedFQNs(snap Snapshot, node NodeKey) map[string]bool {
	vd, ok := snap.Versions[node]
	if !ok {
		return nil
	}
	owned := map[string]bool{}
	for _, p := range vd.OwnedPaths {
		owned[p] = true
	}
	refs := map[string]bool{}
	vd.Files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !owned[fd.Path()] {
			return true
		}
		collectMessageRefs(fd.Messages(), refs)
		return true
	})
	return refs
}

func collectMessageRefs(msgs protoreflect.MessageDescriptors, refs map[string]bool) {
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		fields := md.Fields()
		for j := 0; j < fields.Len(); j++ {
			fd := fields.Get(j)
			if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
				refs[string(fd.Message().FullName())] = true
			}
			if fd.Kind() == protoreflect.EnumKind {
				refs[string(fd.Enum().FullName())] = true
			}
			if fd.IsMap() {
				if k := fd.MapKey(); k.Kind() == protoreflect.EnumKind {
					refs[string(k.Enum().FullName())] = true
				}
				if v := fd.MapValue(); v.Kind() == protoreflect.MessageKind {
					refs[string(v.Message().FullName())] = true
				} else if v.Kind() == protoreflect.EnumKind {
					refs[string(v.Enum().FullName())] = true
				}
			}
		}
		collectMessageRefs(md.Messages(), refs)
	}
}

// referencesAny returns the sorted intersection of node's referenced FQNs
// and the exposed set.
func referencesAny(snap Snapshot, node NodeKey, exposed map[string]bool) []string {
	refs := referencedFQNs(snap, node)
	var hit []string
	for sym := range exposed {
		if refs[sym] {
			hit = append(hit, sym)
		}
	}
	sort.Strings(hit)
	return bounded(hit)
}

// exposedThrough computes the surface node presents to its consumers once
// it consumes the given symbols: the consumed symbols themselves plus
// every message/enum defined in node's owned files that contains a
// consumed symbol (so a dependent referencing the wrapping message is also
// impacted). Conservative by design — when in doubt the change is
// considered reachable.
func exposedThrough(snap Snapshot, node NodeKey, consumed []string) map[string]bool {
	exposed := map[string]bool{}
	for _, s := range consumed {
		exposed[s] = true
	}
	vd, ok := snap.Versions[node]
	if !ok {
		return exposed
	}
	owned := map[string]bool{}
	for _, p := range vd.OwnedPaths {
		owned[p] = true
	}
	consumedSet := map[string]bool{}
	for _, s := range consumed {
		consumedSet[s] = true
	}
	vd.Files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !owned[fd.Path()] {
			return true
		}
		exposeMessageTree(fd.Messages(), consumedSet, exposed)
		enums := fd.Enums()
		for i := 0; i < enums.Len(); i++ {
			ed := enums.Get(i)
			if consumedSet[string(ed.FullName())] {
				exposed[string(ed.FullName())] = true
			}
		}
		return true
	})
	return exposed
}

// exposeMessageTree marks a message when it or any nested message field
// references a consumed symbol; nested messages are marked independently.
func exposeMessageTree(msgs protoreflect.MessageDescriptors, consumed map[string]bool, exposed map[string]bool) {
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		if md.IsMapEntry() {
			continue
		}
		hits := false
		if consumed[string(md.FullName())] {
			hits = true
		}
		fields := md.Fields()
		for j := 0; j < fields.Len(); j++ {
			fd := fields.Get(j)
			if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
				if consumed[string(fd.Message().FullName())] {
					hits = true
				}
			}
			if fd.Kind() == protoreflect.EnumKind && consumed[string(fd.Enum().FullName())] {
				hits = true
			}
		}
		if hits {
			exposed[string(md.FullName())] = true
		}
		exposeMessageTree(md.Messages(), consumed, exposed)
	}
}

// impactedPaths enumerates distinct lock chains from node down to base
// along which taint actually flows, annotating the final (direct) edge.
// Path collection is bounded and de-duplicated by its rendered node
// sequence.
func impactedPaths(snap Snapshot, rev map[NodeKey][]NodeKey, node, base NodeKey,
	directSyms map[NodeKey][]string, taint map[NodeKey]map[string]bool,
	statusOf map[NodeKey]Status, verdictOf map[NodeKey]compat.Verdict) []ReasonPath {

	seen := map[string]bool{}
	var out []ReasonPath
	var dfs func(cur NodeKey, chain []NodeKey, edges []EdgeReason)
	dfs = func(cur NodeKey, chain []NodeKey, edges []EdgeReason) {
		if len(out) >= maxReasonPaths {
			return
		}
		if cur == base {
			key := pathKey(chain)
			if seen[key] {
				return
			}
			seen[key] = true
			strs := make([]string, len(chain))
			for i, k := range chain {
				strs[i] = k.String()
			}
			out = append(out, ReasonPath{Nodes: append([]NodeKey(nil), chain...), Path: strs, Edges: append([]EdgeReason(nil), edges...)})
			return
		}
		// Follow cur's forward lock edges through impacted nodes only.
		vd := snap.Versions[cur]
		nexts := make([]NodeKey, 0, len(vd.Locks))
		for _, l := range vd.Locks {
			nexts = append(nexts, NodeKey{Package: l.Package, Version: l.Version})
		}
		sort.Slice(nexts, func(i, j int) bool {
			if nexts[i].Package != nexts[j].Package {
				return nexts[i].Package < nexts[j].Package
			}
			return nexts[i].Version < nexts[j].Version
		})
		for _, next := range nexts {
			if _, exists := snap.Versions[next]; !exists {
				continue
			}
			repeat := false
			for _, c := range chain {
				if c == next {
					repeat = true
					break
				}
			}
			if repeat {
				continue
			}
			var edge EdgeReason
			if next == base {
				syms := directSyms[cur]
				if len(syms) == 0 {
					continue // this lock edge does not consume the change
				}
				edge = EdgeReason{From: cur.String(), To: next.String(), Kind: "DIRECT", Verdict: verdictOf[cur], Symbols: bounded(append([]string(nil), syms...))}
			} else {
				if _, tainted := taint[next]; !tainted {
					continue
				}
				syms := referencesAny(snap, cur, taint[next])
				if len(syms) == 0 {
					continue
				}
				edge = EdgeReason{From: cur.String(), To: next.String(), Kind: "TRANSIT", Verdict: verdictOf[next], Symbols: syms}
			}
			dfs(next, append(chain, next), append(edges, edge))
		}
	}
	dfs(node, []NodeKey{node}, nil)
	if len(out) == 0 {
		// Defensive: an impacted node must have at least one chain.
		out = []ReasonPath{{Path: []string{node.String(), base.String()}, Edges: []EdgeReason{{From: node.String(), To: base.String(), Kind: "DIRECT", Verdict: verdictOf[node]}}}}
	}
	return out
}

// verifiedPaths collects up to maxReasonPaths lock chains from a
// verified-unaffected node down to base as evidence of what was checked.
func verifiedPaths(snap Snapshot, rev map[NodeKey][]NodeKey, node, base NodeKey) []ReasonPath {
	seen := map[string]bool{}
	var out []ReasonPath
	var dfs func(cur NodeKey, chain []NodeKey)
	dfs = func(cur NodeKey, chain []NodeKey) {
		if len(out) >= maxReasonPaths {
			return
		}
		if cur == base {
			key := pathKey(chain)
			if seen[key] {
				return
			}
			seen[key] = true
			strs := make([]string, len(chain))
			for i, k := range chain {
				strs[i] = k.String()
			}
			edges := make([]EdgeReason, 0, len(chain)-1)
			for i := 0; i+1 < len(chain); i++ {
				edges = append(edges, EdgeReason{From: chain[i].String(), To: chain[i+1].String(), Kind: "VERIFIED_CLEAN", Verdict: compat.VerdictCompatible})
			}
			out = append(out, ReasonPath{Nodes: append([]NodeKey(nil), chain...), Path: strs, Edges: edges})
			return
		}
		vd := snap.Versions[cur]
		for _, l := range vd.Locks {
			next := NodeKey{Package: l.Package, Version: l.Version}
			if _, ok := snap.Versions[next]; !ok {
				continue
			}
			repeat := false
			for _, c := range chain {
				if c == next {
					repeat = true
					break
				}
			}
			if repeat {
				continue
			}
			dfs(next, append(chain, next))
		}
	}
	dfs(node, []NodeKey{node})
	return out
}

func pathKey(chain []NodeKey) string {
	out := ""
	for i, k := range chain {
		if i > 0 {
			out += ">"
		}
		out += k.String()
	}
	return out
}

func bounded(in []string) []string {
	const maxSyms = 16
	if len(in) > maxSyms {
		return in[:maxSyms]
	}
	return in
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	out := append([]string(nil), a...)
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return bounded(out)
}

func cloneKeys(m map[NodeKey]map[string]bool) []NodeKey {
	out := make([]NodeKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Version < out[j].Version
	})
	return out
}
