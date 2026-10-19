package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Every resource that takes architecture must send it as
// infrastructure.architecture on create and on update, in a shape the
// generated request types keep.
func TestArchitecturePayloads(t *testing.T) {
	ctx := t.Context()
	arm := types.StringValue("arm")

	t.Run("deployment service", func(t *testing.T) {
		plan := servicePlan(t)
		plan.Architecture = arm
		body := architectureBody(t)(deploymentServiceRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsServicesDeploymentJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsServicesDeploymentJSONRequestBody{})
	})

	t.Run("build service", func(t *testing.T) {
		plan := buildServicePlan(t)
		plan.Architecture = arm
		body := architectureBody(t)(buildServiceRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsServicesBuildJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsServicesBuildJSONRequestBody{})
	})

	t.Run("combined service", func(t *testing.T) {
		plan := combinedServicePlan(t)
		plan.Architecture = arm
		body := architectureBody(t)(combinedServiceRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsServicesCombinedJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsServicesCombinedJSONRequestBody{})
	})

	t.Run("job", func(t *testing.T) {
		plan := jobPlan(t)
		plan.Architecture = arm
		body := architectureBody(t)(jobRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsJobsJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsJobsJSONRequestBody{})
	})
}

// architectureBody checks that a request body carries the architecture the
// tests set.
func architectureBody(t *testing.T) func([]byte, diag.Diagnostics) []byte {
	return func(body []byte, diags diag.Diagnostics) []byte {
		t.Helper()

		if diags.HasError() {
			t.Fatal(diags)
		}

		var got struct {
			Infrastructure *struct {
				Architecture string `json:"architecture"`
			} `json:"infrastructure"`
		}

		json.Unmarshal(body, &got)

		if got.Infrastructure == nil || got.Infrastructure.Architecture != "arm" {
			t.Errorf("infrastructure = %+v in %s, want architecture arm", got.Infrastructure, body)
		}

		return body
	}
}

// Unset, the infrastructure object is left out, so that Northflank applies
// its default.
func TestArchitectureOmittedWhenUnset(t *testing.T) {
	plan := servicePlan(t)
	plan.Architecture = types.StringUnknown()
	body, diags := deploymentServiceRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	var got map[string]any
	json.Unmarshal(body, &got)

	if _, ok := got["infrastructure"]; ok {
		t.Errorf("body = %s, want no infrastructure", body)
	}
}

func TestServiceArchitectureRead(t *testing.T) {
	var service serviceRead
	json.Unmarshal([]byte(`{"id":"api","infrastructure":{"architecture":"arm"}}`), &service)

	if got := serviceArchitectureValue(&service); got.ValueString() != "arm" {
		t.Errorf("architecture = %v, want arm", got)
	}

	if got := serviceArchitectureValue(&serviceRead{}); !got.IsNull() {
		t.Errorf("architecture = %v, want null when the service reports none", got)
	}
}
