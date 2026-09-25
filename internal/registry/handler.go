package registry

import (
	"encoding/json"
	"net/http"

	"connectrpc.com/connect"
)

// Service procedure paths, matching api/registry/v1/registry.proto.
const (
	ProcedureRegisterVersion    = "/registry.v1.Registry/RegisterVersion"
	ProcedureCheckCompatibility = "/registry.v1.Registry/CheckCompatibility"
	ProcedureDeclareConsumer    = "/registry.v1.Registry/DeclareConsumer"
	ProcedureListVersions       = "/registry.v1.Registry/ListVersions"
)

// jsonCodec speaks application/json for plain Go structs, so the service
// needs no code generation step while remaining a first-class ConnectRPC
// endpoint (connect protocol, and gRPC/gRPC-Web with the json sub-codec).
type jsonCodec struct{}

func (jsonCodec) Name() string { return "json" }

func (jsonCodec) Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// Handler returns the HTTP handler for the registry service.
func (s *Service) Handler() (string, http.Handler) {
	mux := http.NewServeMux()
	mux.Handle(ProcedureRegisterVersion, connect.NewUnaryHandler(
		ProcedureRegisterVersion, s.RegisterVersion, connect.WithCodec(jsonCodec{})))
	mux.Handle(ProcedureCheckCompatibility, connect.NewUnaryHandler(
		ProcedureCheckCompatibility, s.CheckCompatibility, connect.WithCodec(jsonCodec{})))
	mux.Handle(ProcedureDeclareConsumer, connect.NewUnaryHandler(
		ProcedureDeclareConsumer, s.DeclareConsumer, connect.WithCodec(jsonCodec{})))
	mux.Handle(ProcedureListVersions, connect.NewUnaryHandler(
		ProcedureListVersions, s.ListVersions, connect.WithCodec(jsonCodec{})))
	return "/registry.v1.Registry/", mux
}
