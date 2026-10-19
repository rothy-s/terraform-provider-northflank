package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Every resource that takes stage_id must send it as stageId on create and on
// update, in a shape the generated request types -- which follow the spec --
// keep.
func TestStageIDPayloads(t *testing.T) {
	ctx := t.Context()
	stage := types.StringValue("qa-100")

	t.Run("deployment service", func(t *testing.T) {
		plan := servicePlan(t)
		plan.StageID = stage
		body := stageBody(t)(deploymentServiceRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsServicesDeploymentJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsServicesDeploymentJSONRequestBody{})
	})

	t.Run("build service", func(t *testing.T) {
		plan := buildServicePlan(t)
		plan.StageID = stage
		body := stageBody(t)(buildServiceRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsServicesBuildJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsServicesBuildJSONRequestBody{})
	})

	t.Run("combined service", func(t *testing.T) {
		plan := combinedServicePlan(t)
		plan.StageID = stage
		body := stageBody(t)(combinedServiceRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsServicesCombinedJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsServicesCombinedJSONRequestBody{})
	})

	t.Run("job", func(t *testing.T) {
		plan := jobPlan(t)
		plan.StageID = stage
		body := stageBody(t)(jobRequestBody(ctx, &plan))

		roundTrip(t, body, &northflank.PostProjectsJobsJSONRequestBody{})
		roundTrip(t, body, &northflank.PutProjectsJobsJSONRequestBody{})
	})

	t.Run("addon", func(t *testing.T) {
		plan, state := addonModel("16", 1, 4096), addonModel("16", 1, 4096)
		plan.model.StageID = stage
		var diags diag.Diagnostics
		body, _ := json.Marshal(addonPatchFrom(ctx, &plan.model, &state.model, &diags))

		if string(body) != `{"stageId":"qa-100"}` {
			t.Errorf("patch = %s, want only the stage", body)
		}

		roundTrip(t, body, &northflank.PatchProjectsAddonsAddonidJSONRequestBody{})

		state.model.StageID = stage

		if patch := addonPatchFrom(ctx, &plan.model, &state.model, &diags); !patch.empty() {
			t.Errorf("patch for an addon already in the stage is not empty")
		}
	})
}

// stageBody checks that a request body carries the stage the tests set.
func stageBody(t *testing.T) func([]byte, diag.Diagnostics) []byte {
	return func(body []byte, diags diag.Diagnostics) []byte {
		t.Helper()

		if diags.HasError() {
			t.Fatal(diags)
		}

		var got struct {
			StageID *string `json:"stageId"`
		}

		json.Unmarshal(body, &got)

		if got.StageID == nil || *got.StageID != "qa-100" {
			t.Errorf("stageId = %v in %s, want qa-100", got.StageID, body)
		}

		return body
	}
}

// Unset, stage_id is left out, so that Northflank picks the default
// environment for a new resource.
func TestStageIDOmittedWhenUnset(t *testing.T) {
	plan := servicePlan(t)
	plan.StageID = types.StringUnknown()
	body, diags := deploymentServiceRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	var got map[string]any
	json.Unmarshal(body, &got)

	if _, ok := got["stageId"]; ok {
		t.Errorf("body = %s, want no stageId", body)
	}
}

func TestStageIDRead(t *testing.T) {
	var service serviceRead
	json.Unmarshal([]byte(`{"id":"api","stageId":"qa-100","serviceType":"deployment"}`), &service)

	if service.StageID == nil || *service.StageID != "qa-100" {
		t.Errorf("service stage = %v", service.StageID)
	}

	addon, err := addonFromBody([]byte(`{"data":{"id":"db","stageId":"qa-100","spec":{"type":"postgresql"}}}`))

	if err != nil {
		t.Fatal(err)
	}

	if addon.StageID == nil || *addon.StageID != "qa-100" {
		t.Errorf("addon stage = %v", addon.StageID)
	}
}

// Mount contents go out base64-encoded, and only when configured.
func TestDockerSecretMountsPayload(t *testing.T) {
	ctx := t.Context()
	plan := planModel(t)
	plan.DockerSecretMounts = types.MapValueMust(types.StringType, map[string]attr.Value{
		"oban_auth_key": types.StringValue("secret"),
	})

	payload, diags := secretGroupPayloadFrom(ctx, &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	mounts, _ := json.Marshal(payload.Secrets.DockerSecretMounts)

	if string(mounts) != `{"oban_auth_key":{"data":"c2VjcmV0","encoding":"utf-8"}}` {
		t.Errorf("dockerSecretMounts = %s", mounts)
	}

	payload.Name = "App Secrets"
	body, _ := json.Marshal(payload)
	roundTrip(t, body, &northflank.PostProjectsSecretsJSONRequestBody{})

	plan.DockerSecretMounts = types.MapNull(types.StringType)
	payload, _ = secretGroupPayloadFrom(ctx, &plan)
	secrets, _ := json.Marshal(payload.Secrets)

	if string(secrets) != `{"variables":{"DATABASE_URL":"postgres://x"}}` {
		t.Errorf("secrets = %s, want no mounts when none are configured", secrets)
	}
}

// The API reports the build service a deployment service runs as nfObjectId,
// although the spec documents id.
func TestServiceDeploymentValueReadsNfObjectID(t *testing.T) {
	var service serviceRead
	json.Unmarshal([]byte(`{"serviceType":"deployment","deployment":{"instances":1,
		"internal":{"nfObjectId":"builder","nfObjectType":"service","branch":"main","buildSHA":"latest"}}}`), &service)

	value, diags := serviceDeploymentValue(t.Context(), types.ObjectNull(deploymentSourceAttrTypes), &service)

	if diags.HasError() {
		t.Fatal(diags)
	}

	var got deploymentSourceModel
	value.As(t.Context(), &got, objectAsOptions)

	var build buildServiceModel
	got.BuildService.As(t.Context(), &build, objectAsOptions)

	if build.ID.ValueString() != "builder" {
		t.Errorf("build_service.id = %v, want builder", build.ID)
	}
}
