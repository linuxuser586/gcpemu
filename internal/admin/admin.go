// Package admin holds the OpenAPI description of the /_emu/v1/ admin API
// (SRS 7.3), embedded in the binary for `gcpemu admin openapi`.
package admin

import _ "embed"

// OpenAPI is the admin API's OpenAPI 3 document (YAML).
//
//go:embed openapi.yaml
var OpenAPI []byte
