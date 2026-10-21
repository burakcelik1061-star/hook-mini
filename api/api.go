// Package api embeds the OpenAPI document that describes Hook Mini's HTTP API
// and the webhook it sends. The document is served at GET /openapi.yaml and is
// checked against the real routes and responses by the contract tests, so it
// cannot silently drift from the code.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document, in YAML.
//
//go:embed openapi.yaml
var OpenAPI []byte
