package registry

import _ "embed"

// SchemaSQL is the DDL for the registry database. The server applies it
// idempotently at startup; it is also the canonical reference for manual
// provisioning.
//
//go:embed schema.sql
var SchemaSQL string
