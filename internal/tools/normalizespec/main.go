// Command normalizespec preprocesses the vendored Northflank OpenAPI document
// so that oapi-codegen can consume it.
//
// The upstream document at https://api.northflank.com/v1/swagger-json claims
// to be OpenAPI 3.0.1, but is partly written in Swagger 2.0 and OpenAPI 3.1
// idioms that 3.0 tooling rejects:
//
//   - Every operation is a bare {"$ref": "#/components/pathItems/..."}.
//     Operations cannot be references in 3.0, and components.pathItems only
//     exists in 3.1, so the referenced operations are inlined.
//   - Responses carry "schema" directly, as in Swagger 2.0, rather than under
//     content["application/json"].
//   - Parameters repeat their schema keywords ("type", "enum", ...) next to
//     "schema", as in Swagger 2.0, and the schema in turn repeats the
//     parameter's own fields ("name", "in", and a boolean "required", which a
//     3.0 schema cannot have). Both sets of duplicates are dropped.
//   - Parameters list their "examples" as an array, the 3.1 schema form, where
//     3.0 expects a map of Example Objects. The first becomes "example".
//   - Most routes exist twice, as /v1/... and /v1/teams/{teamId}/... (the
//     latter for org-scoped tokens), and both copies share one operationId,
//     which collides in codegen. The team-scoped twins are dropped: an API
//     token belongs to a team, and the /v1/... form addresses that team
//     implicitly. Routes that exist only under /v1/teams/{teamId} are kept.
//     Dropping the twins also halves the size of the generated client.
//   - A handful of operations declare a path parameter their route does not
//     contain. Those parameters are dropped.
//   - Maps keyed by file path are written as an object with a single
//     placeholder property, "/dir/fileName", which codegen turns into an
//     unexported struct field. Those become additionalProperties.
//   - Request bodies that are a bare oneOf/anyOf are inlined. oapi-codegen
//     then emits "type XJSONRequestBody XJSONBody", a defined type that drops
//     the union's MarshalJSON, so the generated client sends "{}". Moving
//     those schemas into components.schemas turns the request body type into
//     an alias, which keeps the method.
//   - The server URL is a placeholder.
//
// The vendored input document is left untouched; the normalized copy is
// written alongside it and is the file codegen actually reads.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// DefaultServerURL replaces the placeholder server in the upstream document.
const DefaultServerURL = "https://api.northflank.com"

// parameterKeys are the fields a 3.0 Parameter Object may carry. Anything else
// on a parameter is a Swagger 2.0 leftover that duplicates its schema.
var parameterKeys = map[string]bool{
	"name":            true,
	"in":              true,
	"description":     true,
	"required":        true,
	"deprecated":      true,
	"allowEmptyValue": true,
	"style":           true,
	"explode":         true,
	"allowReserved":   true,
	"schema":          true,
	"example":         true,
	"examples":        true,
	"content":         true,
}

// parameterOnlyKeys belong to the Parameter Object and are dropped when they
// turn up inside its schema.
var parameterOnlyKeys = []string{"name", "in", "required", "allowEmptyValue", "style", "explode", "allowReserved"}

// placeholderProperties stand for "any key" in the upstream document. An
// object whose only property is one of these is really a map.
var placeholderProperties = map[string]bool{"/dir/fileName": true}

// teamScopedPrefix introduces the team-scoped copy of a route.
const teamScopedPrefix = "/v1/teams/{teamId}"

// httpMethods are the keys of a Path Item that hold operations.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

func main() {
	if len(os.Args) != 3 {
		_, _ = fmt.Fprintf(os.Stderr, "usage: %s <input.json> <output.json>\n", os.Args[0])
		os.Exit(2)
	}

	if err := run(os.Args[1], os.Args[2]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "normalizespec: %v\n", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	raw, err := os.ReadFile(in)

	if err != nil {
		return err
	}

	// Decode into a generic map so we round-trip every key we do not care
	// about, rather than modelling the whole OpenAPI document.
	var doc map[string]any

	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", in, err)
	}

	if err := normalize(doc); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")

	if err != nil {
		return err
	}

	return os.WriteFile(out, append(encoded, '\n'), 0o644)
}

func normalize(doc map[string]any) error {
	doc["servers"] = []any{map[string]any{"url": DefaultServerURL, "description": "Production API"}}

	components, _ := doc["components"].(map[string]any)

	if components == nil {
		components = map[string]any{}
		doc["components"] = components
	}

	pathItems, _ := components["pathItems"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)

	if schemas == nil {
		schemas = map[string]any{}
		components["schemas"] = schemas
	}
	paths, _ := doc["paths"].(map[string]any)

	if paths == nil {
		return fmt.Errorf("document has no paths")
	}

	for _, route := range sortedKeys(paths) {
		if isTeamScopedCopy(route, paths) {
			delete(paths, route)
			continue
		}

		item, ok := paths[route].(map[string]any)

		if !ok {
			continue
		}

		for _, method := range sortedKeys(item) {
			if !httpMethods[method] {
				continue
			}

			op, ok := item[method].(map[string]any)

			if !ok {
				continue
			}

			op, err := resolveOperation(op, pathItems)

			if err != nil {
				return fmt.Errorf("%s %s: %w", strings.ToUpper(method), route, err)
			}

			// Operations shared by several routes are the same map, so work on a
			// copy before renaming or pruning anything.
			op = shallowCopy(op)
			normalizeOperation(op, route)

			if err := hoistUnionRequestBody(op, schemas); err != nil {
				return fmt.Errorf("%s %s: %w", strings.ToUpper(method), route, err)
			}

			item[method] = op
		}
	}

	mapPlaceholderObjects(paths)

	// Everything in components.pathItems has been inlined, and the section is
	// not valid in 3.0.
	delete(components, "pathItems")

	return nil
}

// resolveOperation replaces an operation that is only a reference into
// components.pathItems with the operation it points at.
func resolveOperation(op, pathItems map[string]any) (map[string]any, error) {
	ref, ok := op["$ref"].(string)

	if !ok {
		return op, nil
	}

	const prefix = "#/components/pathItems/"

	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported operation reference %q", ref)
	}

	target, ok := pathItems[strings.TrimPrefix(ref, prefix)].(map[string]any)

	if !ok {
		return nil, fmt.Errorf("dangling operation reference %q", ref)
	}

	return target, nil
}

// isTeamScopedCopy reports whether route is the /v1/teams/{teamId}/... twin
// of another route in the document.
func isTeamScopedCopy(route string, paths map[string]any) bool {
	if !strings.HasPrefix(route, teamScopedPrefix+"/") {
		return false
	}

	_, ok := paths["/v1"+strings.TrimPrefix(route, teamScopedPrefix)]
	return ok
}

func normalizeOperation(op map[string]any, route string) {
	if params, ok := op["parameters"].([]any); ok {
		kept := make([]any, 0, len(params))

		for _, p := range params {
			param, ok := p.(map[string]any)

			if !ok {
				kept = append(kept, p)
				continue
			}

			param = shallowCopy(param)

			if param["in"] == "path" && !strings.Contains(route, "{"+fmt.Sprint(param["name"])+"}") {
				continue
			}

			if examples, ok := param["examples"].([]any); ok {
				delete(param, "examples")

				if _, has := param["example"]; !has && len(examples) > 0 {
					param["example"] = examples[0]
				}
			}

			for key := range param {
				if !parameterKeys[key] && !strings.HasPrefix(key, "x-") {
					delete(param, key)
				}
			}

			if schema, ok := param["schema"].(map[string]any); ok {
				schema = shallowCopy(schema)

				for _, key := range parameterOnlyKeys {
					delete(schema, key)
				}

				param["schema"] = schema
			}

			kept = append(kept, param)
		}

		op["parameters"] = kept
	}

	responses, ok := op["responses"].(map[string]any)

	if !ok {
		return
	}

	responses = shallowCopy(responses)
	op["responses"] = responses

	for code, r := range responses {
		response, ok := r.(map[string]any)

		if !ok {
			continue
		}

		schema, ok := response["schema"]

		if !ok {
			continue
		}

		response = shallowCopy(response)
		delete(response, "schema")
		response["content"] = map[string]any{
			"application/json": map[string]any{"schema": schema},
		}
		responses[code] = response
	}
}

// hoistUnionRequestBody moves a request body schema that is a bare union into
// components.schemas, named after the operation, and references it instead.
func hoistUnionRequestBody(op, schemas map[string]any) error {
	body, ok := op["requestBody"].(map[string]any)

	if !ok {
		return nil
	}

	content, _ := body["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, _ := media["schema"].(map[string]any)

	if schema == nil || (schema["oneOf"] == nil && schema["anyOf"] == nil) {
		return nil
	}

	id, _ := op["operationId"].(string)

	if id == "" {
		return fmt.Errorf("union request body on an operation without an operationId")
	}

	name := strings.ToUpper(id[:1]) + id[1:] + "Request"

	if _, taken := schemas[name]; taken {
		return fmt.Errorf("cannot hoist request body: schema %q already exists", name)
	}

	schemas[name] = schema

	// Copy on the way down: the body may be shared with another route.
	media = shallowCopy(media)
	media["schema"] = map[string]any{"$ref": "#/components/schemas/" + name}
	content = shallowCopy(content)
	content["application/json"] = media
	body = shallowCopy(body)
	body["content"] = content
	op["requestBody"] = body

	return nil
}

// mapPlaceholderObjects rewrites every object schema whose sole property is a
// placeholder into a map of that property's schema. Schemas below the
// operation level are shared between routes and rewritten in place, which is
// safe because the rewrite is the same wherever a schema appears.
func mapPlaceholderObjects(node any) {
	switch n := node.(type) {
	case map[string]any:
		if props, ok := n["properties"].(map[string]any); ok && len(props) == 1 {
			for key, value := range props {
				if placeholderProperties[key] {
					delete(n, "properties")
					delete(n, "required")
					n["additionalProperties"] = value
				}
			}
		}

		for _, v := range n {
			mapPlaceholderObjects(v)
		}

	case []any:
		for _, v := range n {
			mapPlaceholderObjects(v)
		}
	}
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))

	for k, v := range m {
		out[k] = v
	}

	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	return keys
}
