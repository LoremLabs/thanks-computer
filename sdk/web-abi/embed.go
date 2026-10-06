// Package webabisdk embeds the TxCo Web ABI manifest schema, so `txco`
// validates txco-web.json against the same file the @txco/web-abi
// conformance kit ships.
package webabisdk

import _ "embed"

// ManifestSchema is txco-web.schema.json (JSON Schema, draft 2020-12).
//
//go:embed txco-web.schema.json
var ManifestSchema []byte
