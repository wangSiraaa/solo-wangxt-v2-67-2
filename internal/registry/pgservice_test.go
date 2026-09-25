package registry

import (
	"context"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	_ "github.com/jackc/pgx/v5/stdlib"

	"protocompat/internal/deps"
	"protocompat/internal/schema"
)

// TestPGServiceCrossPackage runs the same cross-package acceptance flow
// as the in-memory tests, against PostgreSQL. It verifies pinned
// compilation, lock summaries, the diamond (one node, two reason paths),
// verified-unaffected classification, memoization and cycle atomicity,
// end to end through the service. Skipped without DATABASE_URL.
func TestPGServiceCrossPackage(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL service test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), SchemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	pfx := "testpgsvc_" + time.Now().Format("20060102150405_000000000") + "_"
	common := pfx + "common"
	billing := pfx + "billing"
	api := pfx + "api"
	tax := pfx + "tax"
	paths := func(p string) string { return pfx + p }

	svc := NewService(NewPGStore(db))
	ctx := context.Background()

	reg := func(pkg, version, path, content string, pins []deps.Pin) {
		t.Helper()
		_, err := svc.RegisterVersion(ctx, connectReq(pkg, version, path, content, pins))
		if err != nil {
			t.Fatalf("register %s@%s: %v", pkg, version, err)
		}
	}
	reg(common, "v1", paths("base/common.proto"), `syntax = "proto3"; package `+common+`;
message Money { int64 units = 1; string currency = 2; }
message Code { string value = 1; }`, nil)
	reg(billing, "v1", paths("middle/invoice.proto"), `syntax = "proto3"; package `+billing+`;
import "`+paths("base/common.proto")+`";
message LineItem { `+common+`.Money amount = 1; string label = 2; }`, nil)
	reg(api, "v1", paths("top/service.proto"), `syntax = "proto3"; package `+api+`;
import "`+paths("middle/invoice.proto")+`";
import "`+paths("base/common.proto")+`";
message Invoice { repeated `+billing+`.LineItem lines = 1; `+common+`.Money total = 2; }`, nil)
	reg(tax, "v1", paths("tax/tax.proto"), `syntax = "proto3"; package `+tax+`;
import "`+paths("base/common.proto")+`";
message Basket { `+common+`.Code code = 1; int64 count = 2; }`, nil)
	reg(common, "v2", paths("base/common.proto"), `syntax = "proto3"; package `+common+`;
message Money { int64 units = 1; int64 currency = 2; }
message Code { string value = 1; }`, nil)

	resp, err := svc.AnalyzeImpact(ctx, connectAnalyzeReq(common, "v1", "v2"))
	if err != nil {
		t.Fatal(err)
	}
	byPkg := map[string]ImpactNode{}
	for _, n := range resp.Msg.Analysis.Nodes {
		byPkg[n.Package] = n
	}
	if byPkg[billing].Status != "DIRECT_IMPACTED" || len(byPkg[billing].ReasonPaths) != 1 {
		t.Fatalf("billing = %+v", byPkg[billing])
	}
	if byPkg[api].Status != "DIRECT_IMPACTED" || len(byPkg[api].ReasonPaths) != 2 {
		t.Fatalf("api diamond = %+v", byPkg[api])
	}
	if byPkg[tax].Status != "VERIFIED_UNAFFECTED" {
		t.Fatalf("tax = %+v", byPkg[tax])
	}

	// Memoization: repeat returns the same stored digest.
	again, err := svc.AnalyzeImpact(ctx, connectAnalyzeReq(common, "v1", "v2"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Msg.InputDigest != resp.Msg.InputDigest {
		t.Fatalf("digest drift: %s vs %s", resp.Msg.InputDigest, again.Msg.InputDigest)
	}

	// Missing dependency is refused and leaves nothing.
	_, err = svc.RegisterVersion(ctx, connectReq(pfx+"ghost", "v1", paths("g/g.proto"),
		`syntax = "proto3"; package `+pfx+`ghost; import "`+paths("nope/nope.proto")+`"; message G {}`, nil))
	if code := errCode(err); code != "failed_precondition" {
		t.Fatalf("missing dep code = %s (%v)", code, err)
	}

	// Package cycle rejected atomically.
	_, err = svc.RegisterVersion(ctx, &connect.Request[RegisterVersionRequest]{
		Msg: &RegisterVersionRequest{Package: common, Version: "v3", Files: []schema.SourceFile{
			{Path: paths("base/common.proto"), Content: `syntax = "proto3"; package ` + common + `;
message Money { int64 units = 1; int64 currency = 2; } message Code { string value = 1; }`},
			{Path: paths("base/ref.proto"), Content: `syntax = "proto3"; package ` + common + `;
import "` + paths("middle/invoice.proto") + `";
message Ref { ` + billing + `.LineItem item = 1; }`},
		}},
	})
	if code := errCode(err); code != "failed_precondition" || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle code = %s (%v)", code, err)
	}
	if _, err := svc.GetDependencies(ctx, connectDepReq(common, "v3")); err == nil {
		t.Fatal("cyclic version was stored")
	}
	// The path claim rolled back.
	if _, _, err := svc.store.PathOwner(ctx, paths("base/ref.proto")); err != ErrNotFound {
		t.Fatalf("cyclic path persisted: %v", err)
	}

	// Lock summary digest is stable and populated.
	dep, err := svc.GetDependencies(ctx, connectDepReq(billing, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if dep.Msg.Summary.Digest == "" || len(dep.Msg.Summary.Locks) != 1 {
		t.Fatalf("summary = %+v", dep.Msg.Summary)
	}
	if _, err := hex.DecodeString(dep.Msg.Summary.Locks[0].Digest); err != nil {
		t.Fatalf("lock digest not hex: %v", err)
	}

	// Idempotent retry with a pin that contradicts stored history is
	// refused even though the submitted content is byte-identical.
	_, err = svc.RegisterVersion(ctx, connectReq(billing, "v1", paths("middle/invoice.proto"),
		`syntax = "proto3"; package `+billing+`;
import "`+paths("base/common.proto")+`";
message LineItem { `+common+`.Money amount = 1; string label = 2; }`,
		[]deps.Pin{{Package: common, Version: "v2"}}))
	if code := errCode(err); code != "failed_precondition" {
		t.Fatalf("pin-rewrite retry code = %s (%v)", code, err)
	}
	// Matching pin retries idempotently.
	matchResp, err := svc.RegisterVersion(ctx, connectReq(billing, "v1", paths("middle/invoice.proto"),
		`syntax = "proto3"; package `+billing+`;
import "`+paths("base/common.proto")+`";
message LineItem { `+common+`.Money amount = 1; string label = 2; }`,
		[]deps.Pin{{Package: common, Version: "v1"}}))
	if err != nil || !matchResp.Msg.AlreadyExisted {
		t.Fatalf("matching pin retry: err=%v existed=%v", err, matchResp != nil && matchResp.Msg.AlreadyExisted)
	}
}
