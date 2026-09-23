package provider

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The round-trip tests prove the generated types keep every field of a
// payload, but the generated types do not enforce the spec's constraints:
// patterns, enums, required fields, additionalProperties. These tests check
// payloads against the spec's own schemas, which is what Northflank does
// before it rejects a request with "Request failed payload validation".

var (
	specOnce sync.Once
	specDoc  *openapi3.T
	specErr  error
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()

	specOnce.Do(func() {
		specDoc, specErr = openapi3.NewLoader().LoadFromFile("../../api/openapi.normalized.json")
	})

	if specErr != nil {
		t.Fatalf("loading the spec: %v", specErr)
	}

	return specDoc
}

// validateRequestBody checks body against the JSON request body schema of an
// operation.
func validateRequestBody(t *testing.T, path, method string, body []byte) {
	t.Helper()

	item := loadSpec(t).Paths.Find(path)

	if item == nil {
		t.Fatalf("no path %s in the spec", path)
	}

	op := item.GetOperation(method)

	if op == nil || op.RequestBody == nil || op.RequestBody.Value == nil {
		t.Fatalf("no request body for %s %s", method, path)
	}

	schema := op.RequestBody.Value.Content.Get("application/json").Schema.Value

	var value any

	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}

	// Patterns are skipped: several are JavaScript regular expressions with
	// lookaheads, which Go cannot compile. Everything else is checked.
	if err := schema.VisitJSON(value, openapi3.MultiErrors(), openapi3.DisablePatternValidation()); err != nil {
		t.Errorf("%s %s: the payload fails the spec's validation:\n%v\npayload: %s", method, path, err, body)
	}
}

// apiServicePlan mirrors the Connect "api" combined service: built from
// Git with a Dockerfile, one public HTTP port and two health checks.
func apiServicePlan(t *testing.T) combinedServiceModel {
	t.Helper()
	ctx := t.Context()
	login := "gofieldnode"
	one := 1
	live, ready, port := "/health/live", "/health/ready", 8080

	ports, _ := portsValue(ctx, []servicePortPayload{{Name: "http", InternalPort: 8080, Protocol: "HTTP", Public: true}}, types.ListNull(types.ObjectType{AttrTypes: servicePortAttrTypes}))
	checks, _ := healthChecksValue(ctx, []serviceHealthCheckPayload{
		// Northflank only allows successThreshold on readiness probes.
		{Type: "livenessProbe", Protocol: "HTTP", Port: &port, Path: &live,
			InitialDelaySeconds: 5, PeriodSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 3},
		{Type: "readinessProbe", Protocol: "HTTP", Port: &port, Path: &ready,
			InitialDelaySeconds: 5, PeriodSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 3, SuccessThreshold: &one},
	}, types.ListNull(types.ObjectType{AttrTypes: serviceHealthCheckAttrTypes}))

	return combinedServiceModel{
		ProjectID:      types.StringValue("x-connect-lab"),
		Name:           types.StringValue("api"),
		Description:    types.StringNull(),
		Tags:           types.SetValueMust(types.StringType, nil),
		DeploymentPlan: types.StringValue("nf-compute-20"),
		BuildPlan:      types.StringValue("nf-compute-400-16"),
		Instances:      types.Int64Value(1),
		Repository:     repositoryObject(&login),
		Branch:         types.StringValue("master"),
		Build: types.ObjectValueMust(buildSettingsAttrTypes, map[string]attr.Value{
			"dockerfile": types.ObjectValueMust(dockerfileBuildAttrTypes, map[string]attr.Value{
				"path":         types.StringValue("/docker/api.Dockerfile"),
				"work_dir":     types.StringValue("/"),
				"target":       types.StringNull(),
				"build_engine": types.StringValue("buildkit"),
				"use_cache":    types.BoolValue(false),
				"storage_size": types.Int64Null(),
			}),
			"buildpack": types.ObjectNull(buildpackBuildAttrTypes),
		}),
		BuildArguments:     types.MapNull(types.StringType),
		DisableCI:          types.BoolValue(false),
		Command:            types.StringNull(),
		Entrypoint:         types.StringNull(),
		Autoscaling:        types.ObjectNull(autoscalingAttrTypes),
		Ports:              ports,
		HealthChecks:       checks,
		RuntimeEnvironment: types.MapNull(types.StringType),
	}
}

func TestCombinedServicePayloadValidatesAgainstSpec(t *testing.T) {
	plan := apiServicePlan(t)
	body, diags := combinedServiceRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	validateRequestBody(t, "/v1/projects/{projectId}/services/combined", "POST", body)
	validateRequestBody(t, "/v1/projects/{projectId}/services/combined", "PUT", body)
}
