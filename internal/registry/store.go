package registry

import (
	"context"
	"errors"
	"time"

	"protocompat/internal/compat"
)

// ErrVersionConflict is returned when a (package, version) pair already
// exists with different content. Versions are immutable: same version,
// different content is always rejected, never overwritten.
var ErrVersionConflict = errors.New("version already exists with different content")

// ErrNotFound is returned for unknown packages, versions or consumers.
var ErrNotFound = errors.New("not found")

// Version is one immutable registered schema version.
type Version struct {
	Package       string
	Version       string
	ContentHash   []byte
	DescriptorSet []byte
	OwnedPaths    []string
	CreatedAt     time.Time
}

// ConsumerDecl is a consumer's declared usage of a package: which
// messages/fields it reads and over which encoding. Declarations let the
// service project global findings onto the surface a consumer actually
// depends on.
type ConsumerDecl struct {
	Package   string
	Consumer  string
	Encoding  string // "wire", "json" or "both"
	Usages    []Usage
	UpdatedAt time.Time
}

// StoredReport is a persisted compatibility verdict.
type StoredReport struct {
	Package     string
	BaseVersion string
	HeadVersion string
	Report      *compat.Report
	CreatedAt   time.Time
}

// DepLock pins one direct dependency of a registered version to an
// immutable (package, version) pair plus the content digest that pair had
// when the lock was taken. Locks are written atomically with the version
// and never change afterwards: historical versions always see the
// dependencies they were registered against.
type DepLock struct {
	DepPackage string
	DepVersion string
	DepHash    []byte
}

// DependentEdge is one reverse-dependency edge: Package@Version holds a
// lock on some version of the queried package (DepVersion).
type DependentEdge struct {
	Package    string
	Version    string
	DepVersion string
}

// StoredImpact is a persisted impact analysis. The input hash ties the
// result to the exact base/head contents, so re-analyzing the same input
// returns the same stored result.
type StoredImpact struct {
	Package     string
	BaseVersion string
	HeadVersion string
	InputHash   []byte
	Result      *ImpactResult
	CreatedAt   time.Time
}

// Store persists packages, versions, consumer declarations, reports,
// dependency locks and impact analyses.
type Store interface {
	// PutVersion registers an immutable version together with its
	// dependency locks, atomically. It returns created=false when the
	// exact same content was already registered (idempotent retry; locks
	// are left untouched), and ErrVersionConflict when the version exists
	// with different content.
	PutVersion(ctx context.Context, v Version, deps []DepLock) (created bool, err error)
	GetVersion(ctx context.Context, pkg, version string) (*Version, error)
	// LatestVersion returns the most recently registered version.
	LatestVersion(ctx context.Context, pkg string) (*Version, error)
	ListVersions(ctx context.Context, pkg string) ([]Version, error)

	// FindPathOwner resolves an import path to the package version that
	// most recently registered a file at that path.
	FindPathOwner(ctx context.Context, path string) (pkg, version string, err error)
	// GetDeps returns the dependency locks of one registered version
	// (empty when the version has no external dependencies).
	GetDeps(ctx context.Context, pkg, version string) ([]DepLock, error)
	// Dependents returns every lock edge pointing at pkg: which package
	// versions depend on it, and which of its versions they locked.
	Dependents(ctx context.Context, pkg string) ([]DependentEdge, error)

	// GetImpactAnalysis returns the stored analysis for exactly this
	// (package, base, head) input, or ErrNotFound.
	GetImpactAnalysis(ctx context.Context, pkg, baseVersion, headVersion string) (*StoredImpact, error)
	// PutImpactAnalysis stores an analysis. It returns created=false when
	// an analysis for the same input already exists; callers must then
	// use the stored one so repeated analyses return the same result.
	PutImpactAnalysis(ctx context.Context, a StoredImpact) (created bool, err error)

	PutReport(ctx context.Context, rep StoredReport) error

	UpsertConsumer(ctx context.Context, decl ConsumerDecl) error
	GetConsumer(ctx context.Context, pkg, consumer string) (*ConsumerDecl, error)
}
