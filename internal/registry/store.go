package registry

import (
	"context"
	"errors"
	"time"

	"protocompat/internal/compat"
	"protocompat/internal/deps"
)

// ErrVersionConflict is returned when a (package, version) pair already
// exists with different content. Versions are immutable: same version,
// different content is always rejected, never overwritten.
var ErrVersionConflict = errors.New("version already exists with different content")

// ErrNotFound is returned for unknown packages, versions or consumers.
var ErrNotFound = errors.New("not found")

// ErrDependencyCycle is returned by stores when inserting a version would
// close a package-level dependency cycle. The whole registration is
// rolled back atomically.
var ErrDependencyCycle = errors.New("dependency cycle rejected")

// ErrPathConflict is returned when a version claims a file path already
// owned by a different package.
var ErrPathConflict = errors.New("import path already owned by another package")

// Version is one immutable registered schema version.
type Version struct {
	Package       string
	Version       string
	ContentHash   []byte
	DescriptorSet []byte
	OwnedPaths    []string
	// Locks is the resolved dependency set this version was compiled
	// against; LockDigest is deps.Digest(Locks). Both are empty only for
	// versions registered before cross-package locking existed.
	Locks      []deps.Lock
	LockDigest string
	CreatedAt  time.Time
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

// StoredImpact is one content-addressed impact analysis.
type StoredImpact struct {
	// InputDigest is impact.Snapshot.InputDigest(...); the primary key of
	// the memoization. Repeating an identical analysis returns this row.
	InputDigest string
	Package     string
	BaseVersion string
	// Analysis is the JSON-serialized ImpactAnalysis.
	Analysis  []byte
	CreatedAt time.Time
}

// Store persists packages, versions, consumer declarations and reports.
type Store interface {
	// PutVersion registers an immutable version together with its locked
	// dependency set, all in one transaction. It returns created=false
	// when the exact same content was already registered (idempotent
	// retry; the service separately verifies any explicit pins against
	// the stored lock history) and ErrVersionConflict on
	// same-version/different-content. It must reject a write that closes
	// a dependency cycle with ErrDependencyCycle and leave no partial
	// data behind.
	PutVersion(ctx context.Context, v Version) (created bool, err error)
	GetVersion(ctx context.Context, pkg, version string) (*Version, error)
	// LatestVersion returns the most recently registered version.
	LatestVersion(ctx context.Context, pkg string) (*Version, error)
	ListVersions(ctx context.Context, pkg string) ([]Version, error)
	// AllVersions returns every registered version (for graph walks and
	// impact snapshots).
	AllVersions(ctx context.Context) ([]Version, error)
	// PathOwner returns the package that owns an imported file path and
	// the latest registered version of that package. It returns
	// ErrNotFound when no registered package owns the path.
	PathOwner(ctx context.Context, path string) (pkg string, v *Version, err error)

	PutReport(ctx context.Context, rep StoredReport) error

	UpsertConsumer(ctx context.Context, decl ConsumerDecl) error
	GetConsumer(ctx context.Context, pkg, consumer string) (*ConsumerDecl, error)

	// PutImpact stores a memoized analysis keyed by its input digest.
	PutImpact(ctx context.Context, in StoredImpact) error
	// GetImpact returns a previously stored analysis.
	GetImpact(ctx context.Context, inputDigest string) (*StoredImpact, error)
}
