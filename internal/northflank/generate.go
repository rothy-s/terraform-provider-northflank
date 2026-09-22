// Package northflank contains the generated client for the Northflank API
// (https://api.northflank.com/v1) along with thin hand-written helpers on top
// of it.
package northflank

//go:generate go run github.com/srs/terraform-provider-northflank/internal/tools/normalizespec ../../api/openapi.json ../../api/openapi.normalized.json
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen --config=oapi-codegen.yaml ../../api/openapi.normalized.json
