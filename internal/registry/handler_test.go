package registry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPEndToEnd drives the ConnectRPC handler over real HTTP with the
// connect protocol's JSON codec.
func TestHTTPEndToEnd(t *testing.T) {
	svc := NewService(NewMemStore())
	pattern, handler := svc.Handler()
	mux := http.NewServeMux()
	mux.Handle(pattern, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(procedure string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Post(srv.URL+procedure, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp.StatusCode, out
	}

	// Register v1.
	code, out := post(ProcedureRegisterVersion, map[string]any{
		"package": "acme.http",
		"version": "v1",
		"files":   []map[string]string{{"path": "a.proto", "content": `syntax = "proto3"; package acme.http; message M { int32 a = 1; }`}},
	})
	if code != http.StatusOK {
		t.Fatalf("register v1: status %d, body %v", code, out)
	}

	// Same version, different content -> 409-class error with JSON body.
	code, out = post(ProcedureRegisterVersion, map[string]any{
		"package": "acme.http",
		"version": "v1",
		"files":   []map[string]string{{"path": "a.proto", "content": `syntax = "proto3"; package acme.http; message M { int64 a = 1; }`}},
	})
	if code == http.StatusOK {
		t.Fatalf("expected error for conflicting content, got %v", out)
	}
	if out["code"] != "already_exists" {
		t.Fatalf("conflict body = %v", out)
	}

	// Check with inline candidate.
	code, out = post(ProcedureCheckCompatibility, map[string]any{
		"package":      "acme.http",
		"base_version": "v1",
		"candidate_files": []map[string]string{{
			"path": "a.proto", "content": `syntax = "proto3"; package acme.http; message M { string a = 1; }`,
		}},
	})
	if code != http.StatusOK {
		t.Fatalf("check: status %d, body %v", code, out)
	}
	report, ok := out["report"].(map[string]any)
	if !ok {
		t.Fatalf("no report in %v", out)
	}
	if report["verdict"] != "INCOMPATIBLE" {
		t.Fatalf("verdict = %v, want INCOMPATIBLE", report["verdict"])
	}

	// List versions.
	code, out = post(ProcedureListVersions, map[string]any{"package": "acme.http"})
	if code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	if versions, ok := out["versions"].([]any); !ok || len(versions) != 1 {
		t.Fatalf("versions = %v", out["versions"])
	}

	// --- Cross-package lock + impact analysis over HTTP ---
	put := func(procedure string, body any) (int, map[string]any) {
		t.Helper()
		return post(procedure, body)
	}
	// Dependency package dep v1/v2.
	code, out = put(ProcedureRegisterVersion, map[string]any{
		"package": "acme.dep", "version": "v1",
		"files": []map[string]string{{"path": "d/d.proto",
			"content": `syntax = "proto3"; package acme.dep; message D { string a = 1; }`}},
	})
	if code != http.StatusOK {
		t.Fatalf("register dep v1: status %d body %v", code, out)
	}
	// Consumer locks dep v1 and references D.
	code, out = put(ProcedureRegisterVersion, map[string]any{
		"package": "acme.cons", "version": "v1",
		"files": []map[string]string{{"path": "c/c.proto",
			"content": `syntax = "proto3"; package acme.cons; import "d/d.proto"; message C { acme.dep.D d = 1; }`}},
	})
	if code != http.StatusOK {
		t.Fatalf("register cons v1: status %d body %v", code, out)
	}
	if locks, ok := out["locks"].([]any); !ok || len(locks) != 1 {
		t.Fatalf("expected one lock in response, got %v", out["locks"])
	} else {
		lock := locks[0].(map[string]any)
		if lock["package"] != "acme.dep" || lock["version"] != "v1" {
			t.Fatalf("lock = %v", lock)
		}
	}
	// GetDependencies.
	code, out = put(ProcedureGetDependencies, map[string]any{"package": "acme.cons", "version": "v1"})
	if code != http.StatusOK {
		t.Fatalf("get dependencies: status %d body %v", code, out)
	}
	summary := out["summary"].(map[string]any)
	if summary["digest"] == "" || len(summary["locks"].([]any)) != 1 {
		t.Fatalf("summary = %v", summary)
	}
	// Breaking dep v2: string a -> int32 a.
	code, out = put(ProcedureRegisterVersion, map[string]any{
		"package": "acme.dep", "version": "v2",
		"files": []map[string]string{{"path": "d/d.proto",
			"content": `syntax = "proto3"; package acme.dep; message D { int32 a = 1; }`}},
	})
	if code != http.StatusOK {
		t.Fatalf("register dep v2: status %d body %v", code, out)
	}
	// AnalyzeImpact: the consumer must show DIRECT_IMPACTED, and the
	// second call must return the same input_digest.
	code, out = put(ProcedureAnalyzeImpact, map[string]any{
		"package": "acme.dep", "base_version": "v1", "candidate_version": "v2",
	})
	if code != http.StatusOK {
		t.Fatalf("analyze: status %d body %v", code, out)
	}
	digest1 := out["input_digest"].(string)
	an := out["analysis"].(map[string]any)
	if an["verdict"] != "INCOMPATIBLE" {
		t.Fatalf("analysis verdict = %v", an["verdict"])
	}
	nodes := an["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %v", nodes)
	}
	node := nodes[0].(map[string]any)
	if node["package"] != "acme.cons" || node["status"] != "DIRECT_IMPACTED" {
		t.Fatalf("node = %v", node)
	}
	paths := node["reason_paths"].([]any)
	if len(paths) != 1 || len(paths[0].(map[string]any)["edges"].([]any)) != 1 {
		t.Fatalf("reason paths = %v", paths)
	}
	code, out = put(ProcedureAnalyzeImpact, map[string]any{
		"package": "acme.dep", "base_version": "v1", "candidate_version": "v2",
	})
	if code != http.StatusOK {
		t.Fatalf("analyze repeat: status %d", code)
	}
	if out["input_digest"] != digest1 {
		t.Fatalf("memoized digest changed: %s vs %s", out["input_digest"], digest1)
	}

	// Missing dependency: refused failed_precondition and names the path.
	code, out = put(ProcedureRegisterVersion, map[string]any{
		"package": "acme.orphan", "version": "v1",
		"files": []map[string]string{{"path": "o/o.proto",
			"content": `syntax = "proto3"; package acme.orphan; import "ghost/g.proto"; message O {}`}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("missing dep: status %d body %v", code, out)
	}
	if !strings.Contains(out["message"].(string), "ghost/g.proto") {
		t.Fatalf("missing dep message should name the path: %v", out["message"])
	}
}
