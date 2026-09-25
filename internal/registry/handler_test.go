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
