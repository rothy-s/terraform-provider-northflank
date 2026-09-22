package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// upstream is a trimmed-down document in the shape Northflank publishes.
const upstream = `{
  "openapi": "3.0.1",
  "servers": [{"url": "https://placeholder/v1"}],
  "paths": {
    "/v1/projects/{projectId}": {
      "get": {"$ref": "#/components/pathItems/v1-projects-projectId-get"}
    },
    "/v1/teams/{teamId}/projects/{projectId}": {
      "get": {"$ref": "#/components/pathItems/v1-projects-projectId-get-team-scoped"}
    },
    "/v1/teams/{teamId}/members": {
      "get": {"$ref": "#/components/pathItems/v1-teams-members-get"}
    }
  },
  "components": {
    "pathItems": {
      "v1-projects-projectId-get": {
        "operationId": "getProject",
        "parameters": [
          {
            "name": "projectId", "in": "path", "required": true, "type": "string",
            "schema": {"name": "projectId", "in": "path", "required": true, "type": "string"}
          },
          {
            "name": "stray", "in": "path", "required": true,
            "schema": {"type": "string"}
          },
          {
            "name": "timeframe", "in": "query", "examples": ["1h", "1d"],
            "schema": {"type": "string"}
          }
        ],
        "responses": {
          "200": {
            "description": "ok",
            "schema": {
              "type": "object",
              "properties": {
                "files": {
                  "type": "object",
                  "properties": {"/dir/fileName": {"type": "string"}}
                }
              }
            }
          }
        }
      },
      "v1-projects-projectId-get-team-scoped": {"operationId": "getProject", "parameters": [], "responses": {}},
      "v1-teams-members-get": {"operationId": "getTeamMembers", "parameters": [], "responses": {}}
    }
  }
}`

func normalized(t *testing.T) map[string]any {
	t.Helper()

	var doc map[string]any

	if err := json.Unmarshal([]byte(upstream), &doc); err != nil {
		t.Fatal(err)
	}

	if err := normalize(doc); err != nil {
		t.Fatalf("normalize() error = %v", err)
	}

	return doc
}

func dig(t *testing.T, node any, keys ...any) any {
	t.Helper()

	for _, key := range keys {
		switch k := key.(type) {
		case string:
			m, ok := node.(map[string]any)

			if !ok {
				t.Fatalf("expected an object at %q, got %T", k, node)
			}

			node = m[k]

		case int:
			s, ok := node.([]any)

			if !ok || k >= len(s) {
				t.Fatalf("expected an array with index %d, got %v", k, node)
			}

			node = s[k]
		}
	}

	return node
}

func TestNormalizeInlinesOperations(t *testing.T) {
	doc := normalized(t)

	if got := dig(t, doc, "paths", "/v1/projects/{projectId}", "get", "operationId"); got != "getProject" {
		t.Errorf("operationId = %v, want the inlined operation", got)
	}

	if _, ok := dig(t, doc, "components").(map[string]any)["pathItems"]; ok {
		t.Error("components.pathItems survived normalization")
	}
}

func TestNormalizeDropsTeamScopedTwins(t *testing.T) {
	paths := dig(t, normalized(t), "paths").(map[string]any)

	if _, ok := paths["/v1/teams/{teamId}/projects/{projectId}"]; ok {
		t.Error("team-scoped twin was kept")
	}

	if _, ok := paths["/v1/teams/{teamId}/members"]; !ok {
		t.Error("team-only route was dropped")
	}
}

func TestNormalizeParameters(t *testing.T) {
	params := dig(t, normalized(t), "paths", "/v1/projects/{projectId}", "get", "parameters").([]any)

	if len(params) != 2 {
		t.Fatalf("got %d parameters, want 2 -- the stray path parameter should be gone", len(params))
	}

	want := map[string]any{
		"name": "projectId", "in": "path", "required": true,
		"schema": map[string]any{"type": "string"},
	}

	if !reflect.DeepEqual(params[0], want) {
		t.Errorf("parameter = %v, want %v", params[0], want)
	}

	if got := dig(t, params[1], "example"); got != "1h" {
		t.Errorf("example = %v, want the first of the examples array", got)
	}

	if _, ok := params[1].(map[string]any)["examples"]; ok {
		t.Error("examples array survived normalization")
	}
}

func TestNormalizeResponses(t *testing.T) {
	response := dig(t, normalized(t), "paths", "/v1/projects/{projectId}", "get", "responses", "200").(map[string]any)

	if _, ok := response["schema"]; ok {
		t.Error("response schema was not moved under content")
	}

	files := dig(t, response, "content", "application/json", "schema", "properties", "files").(map[string]any)

	if _, ok := files["properties"]; ok {
		t.Error("placeholder property survived normalization")
	}

	if !reflect.DeepEqual(files["additionalProperties"], map[string]any{"type": "string"}) {
		t.Errorf("additionalProperties = %v, want the placeholder's schema", files["additionalProperties"])
	}
}

func TestNormalizeServer(t *testing.T) {
	if got := dig(t, normalized(t), "servers", 0, "url"); got != DefaultServerURL {
		t.Errorf("server url = %v, want %v", got, DefaultServerURL)
	}
}

func TestNormalizeHoistsUnionRequestBodies(t *testing.T) {
	var doc map[string]any

	err := json.Unmarshal([]byte(`{
	  "paths": {
	    "/v1/projects": {
	      "post": {
	        "operationId": "postProjects",
	        "requestBody": {"content": {"application/json": {"schema": {"oneOf": [{"type": "object"}, {"type": "object"}]}}}},
	        "responses": {}
	      },
	      "patch": {
	        "operationId": "patchProjects",
	        "requestBody": {"content": {"application/json": {"schema": {"type": "object"}}}},
	        "responses": {}
	      }
	    }
	  }
	}`), &doc)

	if err != nil {
		t.Fatal(err)
	}

	if err := normalize(doc); err != nil {
		t.Fatalf("normalize() error = %v", err)
	}

	route := []any{"paths", "/v1/projects"}
	schemaPath := []any{"requestBody", "content", "application/json", "schema"}

	post := dig(t, doc, append(append(route, "post"), schemaPath...)...)

	if !reflect.DeepEqual(post, map[string]any{"$ref": "#/components/schemas/PostProjectsRequest"}) {
		t.Errorf("union body = %v, want a reference to the hoisted schema", post)
	}

	if dig(t, doc, "components", "schemas", "PostProjectsRequest", "oneOf") == nil {
		t.Error("hoisted schema is missing from components.schemas")
	}

	patch := dig(t, doc, append(append(route, "patch"), schemaPath...)...)

	if !reflect.DeepEqual(patch, map[string]any{"type": "object"}) {
		t.Errorf("plain body = %v, want it left inline", patch)
	}
}
