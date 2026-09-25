package registry

import (
	"connectrpc.com/connect"

	"protocompat/internal/deps"
	"protocompat/internal/schema"
)

func connectReq(pkg, version, path, content string, pins []deps.Pin) *connect.Request[RegisterVersionRequest] {
	return connect.NewRequest(&RegisterVersionRequest{
		Package: pkg, Version: version,
		Files: []schema.SourceFile{{Path: path, Content: content}},
		Pins:  pins,
	})
}

func connectAnalyzeReq(pkg, base, candidate string) *connect.Request[AnalyzeImpactRequest] {
	return connect.NewRequest(&AnalyzeImpactRequest{
		Package: pkg, BaseVersion: base, CandidateVersion: candidate,
	})
}

func connectDepReq(pkg, version string) *connect.Request[GetDependenciesRequest] {
	return connect.NewRequest(&GetDependenciesRequest{Package: pkg, Version: version})
}

func errCode(err error) string {
	if err == nil {
		return ""
	}
	return connect.CodeOf(err).String()
}
