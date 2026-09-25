package impact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// InputDigest is a content digest of exactly the data an analysis
// depends on: the analyzed (package, base, candidate) triple plus every
// package version in the reverse-reachable subgraph of the base version
// (the only versions that can appear in the result), keyed by
// package@version with its content hash and lock set. Timestamps are
// deliberately excluded.
//
// Repeating an analysis against the same registered content returns the
// same digest — and therefore the same stored result — even after newer
// versions of unrelated packages, or newer versions of the same package
// pinned elsewhere, are registered later: those rows are outside the
// base version's subgraph and never participated.
func (s Snapshot) InputDigest(pkg, base, candidate string) string {
	root := NodeKey{Package: pkg, Version: base}

	// Reverse adjacency over existing lock edges.
	rev := map[NodeKey]map[NodeKey]bool{}
	for k, vd := range s.Versions {
		for _, l := range vd.Locks {
			to := NodeKey{Package: l.Package, Version: l.Version}
			if _, ok := s.Versions[to]; !ok {
				continue
			}
			if to.Package == pkg && to.Version != base {
				continue // pinned to another version: outside this analysis
			}
			if rev[to] == nil {
				rev[to] = map[NodeKey]bool{}
			}
			rev[to][k] = true
		}
	}
	reachable := map[NodeKey]bool{root: true}
	var walk func(NodeKey)
	walk = func(n NodeKey) {
		for up := range rev[n] {
			if !reachable[up] {
				reachable[up] = true
				walk(up)
			}
		}
	}
	walk(root)
	// The candidate itself never participates in the graph walk but its
	// content drives the findings.
	reachable[NodeKey{Package: pkg, Version: candidate}] = true

	type lockJSON struct {
		Package string `json:"package"`
		Version string `json:"version"`
	}
	type versionJSON struct {
		ContentHash string     `json:"content_hash"`
		Locks       []lockJSON `json:"locks"`
	}
	input := struct {
		Package   string                 `json:"package"`
		Base      string                 `json:"base"`
		Candidate string                 `json:"candidate"`
		Versions  map[string]versionJSON `json:"versions"`
	}{
		Package:   pkg,
		Base:      base,
		Candidate: candidate,
		Versions:  map[string]versionJSON{},
	}
	for k := range reachable {
		vd, ok := s.Versions[k]
		if !ok {
			continue
		}
		locks := make([]lockJSON, 0, len(vd.Locks))
		for _, l := range vd.Locks {
			locks = append(locks, lockJSON{Package: l.Package, Version: l.Version})
		}
		sort.Slice(locks, func(i, j int) bool {
			if locks[i].Package != locks[j].Package {
				return locks[i].Package < locks[j].Package
			}
			return locks[i].Version < locks[j].Version
		})
		input.Versions[k.String()] = versionJSON{
			ContentHash: hex.EncodeToString(vd.ContentHash),
			Locks:       locks,
		}
	}
	b, _ := json.Marshal(input)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
