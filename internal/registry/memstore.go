package registry

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"

	"protocompat/internal/deps"
)

// MemStore is an in-memory Store with the same semantics as the
// PostgreSQL store. It backs unit tests and local dry-runs.
type MemStore struct {
	mu        sync.Mutex
	versions  map[string]map[string]*Version // package -> version -> record
	reports   []StoredReport
	consumers map[string]map[string]*ConsumerDecl // package -> consumer -> decl
	// paths maps owned file path -> owning package.
	paths   map[string]string
	impacts map[string]*StoredImpact
}

func NewMemStore() *MemStore {
	return &MemStore{
		versions:  map[string]map[string]*Version{},
		consumers: map[string]map[string]*ConsumerDecl{},
		paths:     map[string]string{},
		impacts:   map[string]*StoredImpact{},
	}
}

func (m *MemStore) PutVersion(_ context.Context, v Version) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pkg, ok := m.versions[v.Package]
	if !ok {
		pkg = map[string]*Version{}
		m.versions[v.Package] = pkg
	}
	if existing, ok := pkg[v.Version]; ok {
		if !bytes.Equal(existing.ContentHash, v.ContentHash) {
			return false, ErrVersionConflict
		}
		// Identical content is always an idempotent retry at the storage
		// layer; service-level pin verification enforces lock-history
		// immutability for explicit pins.
		return false, nil
	}

	// Reject a registration that would close a package dependency cycle.
	graph := m.lockedGraph()
	if cycle := graph.AddCycle(v.Package, v.Locks); cycle != nil {
		return false, &deps.CycleError{Cycle: cycle}
	}

	// Claim owned paths before writing the version: a path owned by a
	// different package aborts without any version row.
	for _, p := range v.OwnedPaths {
		if owner, taken := m.paths[p]; taken && owner != v.Package {
			return false, &pathConflictError{path: p, owner: owner}
		}
	}

	cp := v
	if cp.Locks == nil {
		cp.Locks = []deps.Lock{}
	}
	cp.CreatedAt = time.Now()
	pkg[v.Version] = &cp
	for _, p := range v.OwnedPaths {
		m.paths[p] = v.Package
	}
	return true, nil
}

type pathConflictError struct {
	path, owner string
}

func (e *pathConflictError) Error() string {
	return "import path " + e.path + " already owned by " + e.owner
}

func (e *pathConflictError) Unwrap() error { return ErrPathConflict }

func (m *MemStore) GetVersion(_ context.Context, pkg, version string) (*Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.versions[pkg]; ok {
		if v, ok := p[version]; ok {
			cp := *v
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) LatestVersion(_ context.Context, pkg string) (*Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.versions[pkg]
	if !ok {
		return nil, ErrNotFound
	}
	var latest *Version
	for _, v := range p {
		if latest == nil || v.CreatedAt.After(latest.CreatedAt) {
			latest = v
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	cp := *latest
	return &cp, nil
}

func (m *MemStore) ListVersions(_ context.Context, pkg string) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for _, v := range m.versions[pkg] {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) AllVersions(_ context.Context) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for _, p := range m.versions {
		for _, v := range p {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (m *MemStore) PathOwner(_ context.Context, path string) (string, *Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.paths[path]
	if !ok {
		return "", nil, ErrNotFound
	}
	p, ok := m.versions[owner]
	if !ok {
		return "", nil, ErrNotFound
	}
	var latest *Version
	for _, v := range p {
		if latest == nil || v.CreatedAt.After(latest.CreatedAt) {
			latest = v
		}
	}
	if latest == nil {
		return "", nil, ErrNotFound
	}
	cp := *latest
	return owner, &cp, nil
}

func (m *MemStore) PutReport(_ context.Context, rep StoredReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rep.CreatedAt = time.Now()
	m.reports = append(m.reports, rep)
	return nil
}

func (m *MemStore) UpsertConsumer(_ context.Context, decl ConsumerDecl) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pkg, ok := m.consumers[decl.Package]
	if !ok {
		pkg = map[string]*ConsumerDecl{}
		m.consumers[decl.Package] = pkg
	}
	decl.UpdatedAt = time.Now()
	cp := decl
	pkg[decl.Consumer] = &cp
	return nil
}

func (m *MemStore) GetConsumer(_ context.Context, pkg, consumer string) (*ConsumerDecl, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.consumers[pkg]; ok {
		if d, ok := p[consumer]; ok {
			cp := *d
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) PutImpact(_ context.Context, in StoredImpact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.impacts[in.InputDigest]; exists {
		return nil // idempotent
	}
	in.CreatedAt = time.Now()
	cp := in
	m.impacts[in.InputDigest] = &cp
	return nil
}

func (m *MemStore) GetImpact(_ context.Context, inputDigest string) (*StoredImpact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.impacts[inputDigest]; ok {
		cp := *in
		return &cp, nil
	}
	return nil, ErrNotFound
}

// lockedGraph builds the package-level lock graph from all stored
// versions. The caller must hold m.mu.
func (m *MemStore) lockedGraph() deps.Graph {
	g := deps.Graph{}
	edges := map[string]map[string]bool{}
	for _, p := range m.versions {
		for _, v := range p {
			if edges[v.Package] == nil {
				edges[v.Package] = map[string]bool{}
			}
			for _, l := range v.Locks {
				edges[v.Package][l.Package] = true
			}
		}
	}
	for pkg, set := range edges {
		for dep := range set {
			g[pkg] = append(g[pkg], dep)
		}
	}
	return g
}
