package registry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
}

// TestHTTPDependencyFlow drives the cross-package endpoints over real
// HTTP: register a base and a dependent, inspect the lock, then analyze
// the impact of a breaking base change.
func TestHTTPDependencyFlow(t *testing.T) {
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
	register := func(pkg, version, path, content string) (int, map[string]any) {
		return post(ProcedureRegisterVersion, map[string]any{
			"package": pkg, "version": version,
			"files": []map[string]string{{"path": path, "content": content}},
		})
	}

	code, out := register("acme.base", "v1", "base/common.proto",
		`syntax = "proto3"; package acme.base; message Shared { string id = 1; int32 amount = 2; }`)
	if code != http.StatusOK {
		t.Fatalf("register base v1: %d %v", code, out)
	}
	code, out = register("acme.mid", "v1", "mid/invoice.proto",
		`syntax = "proto3"; package acme.mid; import "base/common.proto"; message Invoice { acme.base.Shared shared = 1; }`)
	if code != http.StatusOK {
		t.Fatalf("register mid v1: %d %v", code, out)
	}

	// The lock is queryable.
	code, out = post(ProcedureListDependencies, map[string]any{"package": "acme.mid", "version": "v1"})
	if code != http.StatusOK {
		t.Fatalf("list dependencies: %d %v", code, out)
	}
	deps, ok := out["dependencies"].([]any)
	if !ok || len(deps) != 1 {
		t.Fatalf("dependencies = %v", out["dependencies"])
	}
	dep := deps[0].(map[string]any)
	if dep["package"] != "acme.base" || dep["version"] != "v1" || dep["content_hash"] == "" {
		t.Fatalf("lock = %v", dep)
	}

	// Breaking change in base; the dependent must show up as DIRECT.
	code, out = register("acme.base", "v2", "base/common.proto",
		`syntax = "proto3"; package acme.base; message Shared { string id = 1; int64 amount = 2; }`)
	if code != http.StatusOK {
		t.Fatalf("register base v2: %d %v", code, out)
	}
	code, out = post(ProcedureAnalyzeImpact, map[string]any{
		"package": "acme.base", "base_version": "v1", "head_version": "v2",
	})
	if code != http.StatusOK {
		t.Fatalf("analyze: %d %v", code, out)
	}
	if out["reused"] != false {
		t.Fatalf("first analysis must compute: %v", out)
	}
	impacts, ok := out["impacts"].([]any)
	if !ok || len(impacts) != 1 {
		t.Fatalf("impacts = %v", out["impacts"])
	}
	impact := impacts[0].(map[string]any)
	if impact["package"] != "acme.mid" || impact["impact"] != "DIRECT" {
		t.Fatalf("impact = %v", impact)
	}

	// Same input again: the stored result is reused.
	code, out = post(ProcedureAnalyzeImpact, map[string]any{
		"package": "acme.base", "base_version": "v1", "head_version": "v2",
	})
	if code != http.StatusOK || out["reused"] != true {
		t.Fatalf("second analysis must reuse the stored result: %d %v", code, out)
	}
}
