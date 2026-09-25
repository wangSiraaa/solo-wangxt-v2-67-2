package registry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"protocompat/internal/deps"
)

// PGStore is the PostgreSQL-backed Store. Packages, immutable versions,
// dependency locks, consumer declarations, compatibility reports and
// impact analyses live here.
type PGStore struct {
	db *sql.DB
}

func NewPGStore(db *sql.DB) *PGStore {
	return &PGStore{db: db}
}

// ensurePackage returns the package id, creating the row if needed.
func (s *PGStore) ensurePackage(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		WITH ins AS (
			INSERT INTO packages (name) VALUES ($1)
			ON CONFLICT (name) DO NOTHING
			RETURNING id
		)
		SELECT id FROM ins
		UNION ALL SELECT id FROM packages WHERE name = $1
		LIMIT 1`, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure package %q: %w", name, err)
	}
	return id, nil
}

func (s *PGStore) PutVersion(ctx context.Context, v Version) (bool, error) {
	owned, err := json.Marshal(v.OwnedPaths)
	if err != nil {
		return false, err
	}
	locks, err := json.Marshal(v.Locks)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	pkgID, err := s.ensurePackage(ctx, tx, v.Package)
	if err != nil {
		return false, err
	}

	// Immutable version: identical content is an idempotent retry,
	// different content is rejected before any lock row is touched.
	var existingHash []byte
	err = tx.QueryRowContext(ctx, `
		SELECT content_hash FROM versions
		WHERE package_id = $1 AND version = $2`, pkgID, v.Version).
		Scan(&existingHash)
	switch {
	case err == nil:
		if !bytes.Equal(existingHash, v.ContentHash) {
			return false, ErrVersionConflict
		}
		// Identical content is an idempotent retry at the storage layer;
		// lock-history immutability for explicit pins is enforced by the
		// service after this call returns.
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("check existing version: %w", err)
	}

	// Resolve every locked dependency up-front (also proves they exist).
	type depRow struct {
		pkgID int64
		hash  []byte
	}
	depRows := make(map[string]depRow, len(v.Locks))
	for _, l := range v.Locks {
		did, err := s.ensurePackage(ctx, tx, l.Package)
		if err != nil {
			return false, err
		}
		var hash []byte
		err = tx.QueryRowContext(ctx, `
			SELECT content_hash FROM versions
			WHERE package_id = $1 AND version = $2`, did, l.Version).Scan(&hash)
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("%w: dependency %s@%s is not registered", ErrNotFound, l.Package, l.Version)
		}
		if err != nil {
			return false, err
		}
		if l.Digest != "" && hex.EncodeToString(hash) != l.Digest {
			return false, fmt.Errorf("dependency %s@%s digest mismatch: stored %s, pin demanded %s",
				l.Package, l.Version, hex.EncodeToString(hash), l.Digest)
		}
		depRows[l.Package] = depRow{pkgID: did, hash: hash}
	}

	// Insert the version row.
	var insertedID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO versions (package_id, version, content_hash, descriptor_set, owned_paths, locks, lock_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		pkgID, v.Version, v.ContentHash, v.DescriptorSet, owned, locks, v.LockDigest).
		Scan(&insertedID); err != nil {
		return false, fmt.Errorf("insert version: %w", err)
	}

	// Claim owned file paths, all within this transaction. A path owned
	// by another package fails the transaction: no version, edges or path
	// rows survive.
	for _, p := range v.OwnedPaths {
		var ownerID int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO package_paths (path, package_id) VALUES ($1, $2)
			ON CONFLICT (path) DO UPDATE SET package_id = package_paths.package_id
			RETURNING package_id`, p, pkgID).Scan(&ownerID)
		if err != nil {
			return false, fmt.Errorf("claim path %q: %w", p, err)
		}
		if ownerID != pkgID {
			return false, fmt.Errorf("%w: packages cannot share file paths", ErrPathConflict)
		}
	}

	// Write dependency edges.
	for _, l := range v.Locks {
		dr := depRows[l.Package]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO version_dependencies
				(version_id, dep_package_id, dep_version, dep_content_hash)
			VALUES ($1, $2, $3, $4)`,
			insertedID, dr.pkgID, l.Version, dr.hash); err != nil {
			return false, fmt.Errorf("insert dependency edge %s@%s: %w", l.Package, l.Version, err)
		}
	}

	// Atomic cycle guard: if the newly inserted version closes a path
	// back to its own package, the CTE finds it and we roll the whole
	// transaction back, leaving no version/edge/path rows behind.
	var cyclePath string
	err = tx.QueryRowContext(ctx, `
		WITH RECURSIVE edges AS (
			SELECT v.package_id AS from_pkg, d.dep_package_id AS to_pkg
			FROM version_dependencies d JOIN versions v ON v.id = d.version_id
		),
		walk(ids, cycle) AS (
			SELECT ARRAY[from_pkg, to_pkg], to_pkg = from_pkg
			FROM edges WHERE from_pkg = $1
			UNION ALL
			SELECT w.ids || e.to_pkg, e.to_pkg = $1
			FROM walk w
			JOIN edges e ON e.from_pkg = w.ids[array_upper(w.ids, 1)]
			WHERE NOT w.cycle
			  AND NOT (e.to_pkg = ANY(w.ids[2:array_upper(w.ids, 1) - 1]))
		)
		SELECT array_to_string(ARRAY(
			SELECT p.name FROM unnest(ids) WITH ORDINALITY AS t(id, ord)
			JOIN packages p ON p.id = t.id ORDER BY t.ord
		), ' -> ')
		FROM walk WHERE cycle LIMIT 1`, pkgID).Scan(&cyclePath)
	switch {
	case err == nil:
		return false, fmt.Errorf("%w: %s", ErrDependencyCycle, cyclePath)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, fmt.Errorf("cycle check: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// scanVersion scans one versions row into a Version.
func scanVersion(row interface {
	Scan(dest ...any) error
}) (*Version, error) {
	var v Version
	var owned, locks []byte
	if err := row.Scan(&v.Package, &v.Version, &v.ContentHash, &v.DescriptorSet, &owned, &v.LockDigest, &locks, &v.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(owned, &v.OwnedPaths); err != nil {
		return nil, fmt.Errorf("decode owned paths: %w", err)
	}
	if err := json.Unmarshal(locks, &v.Locks); err != nil {
		return nil, fmt.Errorf("decode locks: %w", err)
	}
	if v.Locks == nil {
		v.Locks = []deps.Lock{}
	}
	return &v, nil
}

const versionColumns = `p.name, v.version, v.content_hash, v.descriptor_set, v.owned_paths, v.lock_digest, v.locks, v.created_at`

func (s *PGStore) GetVersion(ctx context.Context, pkg, version string) (*Version, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, `
		SELECT `+versionColumns+`
		FROM versions v JOIN packages p ON p.id = v.package_id
		WHERE p.name = $1 AND v.version = $2`, pkg, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *PGStore) LatestVersion(ctx context.Context, pkg string) (*Version, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, `
		SELECT `+versionColumns+`
		FROM versions v JOIN packages p ON p.id = v.package_id
		WHERE p.name = $1
		ORDER BY v.created_at DESC, v.id DESC
		LIMIT 1`, pkg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *PGStore) ListVersions(ctx context.Context, pkg string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+versionColumns+`
		FROM versions v JOIN packages p ON p.id = v.package_id
		WHERE p.name = $1
		ORDER BY v.created_at, v.id`, pkg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func (s *PGStore) AllVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+versionColumns+`
		FROM versions v JOIN packages p ON p.id = v.package_id
		ORDER BY p.name, v.created_at, v.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func (s *PGStore) PathOwner(ctx context.Context, path string) (string, *Version, error) {
	var pkg string
	v, err := scanVersion(s.db.QueryRowContext(ctx, `
		SELECT `+versionColumns+`
		FROM package_paths pp
		JOIN packages p ON p.id = pp.package_id
		JOIN versions v ON v.package_id = p.id
		WHERE pp.path = $1
		ORDER BY v.created_at DESC, v.id DESC
		LIMIT 1`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	pkg = v.Package
	return pkg, v, nil
}

func (s *PGStore) PutReport(ctx context.Context, rep StoredReport) error {
	body, err := json.Marshal(rep.Report)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pkgID, err := s.ensurePackage(ctx, tx, rep.Package)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO reports (package_id, base_version, head_version, verdict, report)
		VALUES ($1, $2, $3, $4, $5)`,
		pkgID, rep.BaseVersion, rep.HeadVersion, string(rep.Report.Verdict), body)
	if err != nil {
		return fmt.Errorf("insert report: %w", err)
	}
	return tx.Commit()
}

func (s *PGStore) UpsertConsumer(ctx context.Context, decl ConsumerDecl) error {
	usages, err := json.Marshal(decl.Usages)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pkgID, err := s.ensurePackage(ctx, tx, decl.Package)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO consumers (package_id, consumer, encoding, usages)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (package_id, consumer)
		DO UPDATE SET encoding = EXCLUDED.encoding, usages = EXCLUDED.usages, updated_at = now()`,
		pkgID, decl.Consumer, decl.Encoding, usages)
	if err != nil {
		return fmt.Errorf("upsert consumer: %w", err)
	}
	return tx.Commit()
}

func (s *PGStore) GetConsumer(ctx context.Context, pkg, consumer string) (*ConsumerDecl, error) {
	var d ConsumerDecl
	var usages []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT p.name, c.consumer, c.encoding, c.usages, c.updated_at
		FROM consumers c JOIN packages p ON p.id = c.package_id
		WHERE p.name = $1 AND c.consumer = $2`, pkg, consumer).
		Scan(&d.Package, &d.Consumer, &d.Encoding, &usages, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(usages, &d.Usages); err != nil {
		return nil, fmt.Errorf("decode usages: %w", err)
	}
	return &d, nil
}

func (s *PGStore) PutImpact(ctx context.Context, in StoredImpact) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pkgID, err := s.ensurePackage(ctx, tx, in.Package)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO impact_analyses (input_digest, package_id, base_version, analysis)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (input_digest) DO NOTHING`,
		in.InputDigest, pkgID, in.BaseVersion, in.Analysis)
	if err != nil {
		return fmt.Errorf("insert impact analysis: %w", err)
	}
	return tx.Commit()
}

func (s *PGStore) GetImpact(ctx context.Context, inputDigest string) (*StoredImpact, error) {
	var in StoredImpact
	var pkgName string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.name, ia.base_version, ia.analysis, ia.created_at
		FROM impact_analyses ia JOIN packages p ON p.id = ia.package_id
		WHERE ia.input_digest = $1`, inputDigest).
		Scan(&pkgName, &in.BaseVersion, &in.Analysis, &in.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	in.InputDigest = inputDigest
	in.Package = pkgName
	return &in, nil
}
