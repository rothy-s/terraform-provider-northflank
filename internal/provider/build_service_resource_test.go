package provider

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func repositoryObject(accountLogin *string) types.Object {
	return types.ObjectValueMust(repositoryAttrTypes, map[string]attr.Value{
		"url":                types.StringValue("https://github.com/acme/api"),
		"provider":           types.StringValue("github"),
		"account_login":      types.StringPointerValue(accountLogin),
		"vcs_link_id":        types.StringNull(),
		"self_hosted_vcs_id": types.StringNull(),
	})
}

func dockerfileBuild(engine string, target *string) types.Object {
	return types.ObjectValueMust(buildSettingsAttrTypes, map[string]attr.Value{
		"dockerfile": types.ObjectValueMust(dockerfileBuildAttrTypes, map[string]attr.Value{
			"path":         types.StringValue("/docker/Dockerfile"),
			"work_dir":     types.StringValue("/"),
			"target":       types.StringPointerValue(target),
			"build_engine": types.StringValue(engine),
			"use_cache":    types.BoolValue(true),
			"storage_size": types.Int64Value(16384),
		}),
		"buildpack": types.ObjectNull(buildpackBuildAttrTypes),
	})
}

func buildServicePlan(t *testing.T) buildServiceResourceModel {
	t.Helper()
	target := "runtime"

	return buildServiceResourceModel{
		ProjectID:          types.StringValue("my-project"),
		Name:               types.StringValue("API Builder"),
		Description:        types.StringValue("Builds the API"),
		Tags:               types.SetValueMust(types.StringType, nil),
		BuildPlan:          types.StringValue("nf-compute-200"),
		Repository:         repositoryObject(nil),
		Build:              dockerfileBuild("buildkit", &target),
		BranchRestrictions: types.ListValueMust(types.StringType, stringValues([]string{"main", "release/*"})),
		PrRestrictions:     types.ListValueMust(types.StringType, nil),
		BuildArguments: types.MapValueMust(types.StringType, map[string]attr.Value{
			"NPM_TOKEN": types.StringValue("secret"),
		}),
		DisableCI: types.BoolValue(false),
	}
}

// roundTrip checks that a generated request type keeps every field of body.
func roundTrip(t *testing.T, body []byte, target any) {
	t.Helper()

	var want map[string]any

	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("the generated type rejects the payload: %v", err)
	}

	again, err := json.Marshal(target)

	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	json.Unmarshal(again, &got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip through %T changed the payload\n got: %v\nwant: %v", target, got, want)
	}
}

func TestBuildServicePayloadMatchesSpec(t *testing.T) {
	plan := buildServicePlan(t)
	body, diags := buildServiceRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	t.Run("create", func(t *testing.T) { roundTrip(t, body, &northflank.PostProjectsServicesBuildJSONRequestBody{}) })
	t.Run("update", func(t *testing.T) { roundTrip(t, body, &northflank.PutProjectsServicesBuildJSONRequestBody{}) })

	var payload map[string]any
	json.Unmarshal(body, &payload)

	config := payload["buildConfiguration"].(map[string]any)

	if config["dockerfileTarget"] != "runtime" || !reflect.DeepEqual(config["prRestrictions"], []any{}) {
		t.Errorf("buildConfiguration = %v, want the target and an explicit empty PR rule list", config)
	}

	if payload["buildSource"] != "git" {
		t.Errorf("buildSource = %v", payload["buildSource"])
	}
}

// The cache setting only exists for buildkit, so kaniko builds must not send it.
func TestBuildSettingsCacheOnlyForBuildkit(t *testing.T) {
	ctx := t.Context()

	settings, _, diags := buildSettingsPayloadFrom(ctx, dockerfileBuild("kaniko", nil))

	if diags.HasError() {
		t.Fatal(diags)
	}

	if settings.Dockerfile.Buildkit != nil {
		t.Errorf("buildkit = %+v, want it omitted for kaniko", settings.Dockerfile.Buildkit)
	}

	settings, _, _ = buildSettingsPayloadFrom(ctx, dockerfileBuild("buildkit", nil))

	if settings.Dockerfile.Buildkit == nil || !settings.Dockerfile.Buildkit.UseCache {
		t.Errorf("buildkit = %+v, want useCache sent", settings.Dockerfile.Buildkit)
	}
}

// responseFor echoes a request body back as the API would report it, with
// the identifiers and the Git account Northflank picked filled in.
func responseFor(t *testing.T, kind string, body []byte) *serviceRead {
	t.Helper()

	var data map[string]any
	json.Unmarshal(body, &data)

	data["id"], data["uid"], data["appId"] = "svc", "68f0aa1b9c4e2d7f11a3b402", "/team/my-project/svc"
	data["serviceType"] = kind
	data["createdAt"] = "2024-05-01T00:00:00Z"
	data["vcsData"].(map[string]any)["accountLogin"] = "picked-by-northflank"

	raw, _ := json.Marshal(map[string]any{"data": data})

	service, err := serviceFromBody(raw)

	if err != nil {
		t.Fatal(err)
	}

	return service
}

func TestApplyBuildServiceRoundTrip(t *testing.T) {
	ctx := t.Context()
	plan := buildServicePlan(t)
	body, _ := buildServiceRequestBody(ctx, &plan)

	model := buildServicePlan(t)

	if diags := applyBuildService(ctx, &model, responseFor(t, serviceKindBuild, body)); diags.HasError() {
		t.Fatal(diags)
	}

	for name, pair := range map[string][2]attr.Value{
		// The account Northflank picked must not surface, or Terraform
		// reports an inconsistent result.
		"repository":          {model.Repository, plan.Repository},
		"build":               {model.Build, plan.Build},
		"branch_restrictions": {model.BranchRestrictions, plan.BranchRestrictions},
		"pr_restrictions":     {model.PrRestrictions, plan.PrRestrictions},
		"build_arguments":     {model.BuildArguments, plan.BuildArguments},
		"build_plan":          {model.BuildPlan, plan.BuildPlan},
		"disable_ci":          {model.DisableCI, plan.DisableCI},
	} {
		if !pair[0].Equal(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
	}

	// After an import there is nothing configured, so the account is recorded.
	imported := buildServiceResourceModel{Repository: types.ObjectNull(repositoryAttrTypes)}
	applyBuildService(ctx, &imported, responseFor(t, serviceKindBuild, body))

	var repo repositoryModel
	imported.Repository.As(ctx, &repo, objectAsOptions)

	if repo.AccountLogin.ValueString() != "picked-by-northflank" {
		t.Errorf("imported account_login = %v, want the reported account", repo.AccountLogin)
	}
}

func combinedServicePlan(t *testing.T) combinedServiceModel {
	t.Helper()
	ctx := t.Context()
	login := "acme-bot"

	ports, _ := portsValue(ctx, []servicePortPayload{{Name: "http", InternalPort: 3000, Protocol: "HTTP", Public: true}})
	checks, _ := healthChecksValue(ctx, nil)

	return combinedServiceModel{
		ProjectID:      types.StringValue("my-project"),
		Name:           types.StringValue("Web"),
		Description:    types.StringValue("Website"),
		Tags:           types.SetValueMust(types.StringType, nil),
		DeploymentPlan: types.StringValue("nf-compute-20"),
		BuildPlan:      types.StringUnknown(),
		Instances:      types.Int64Value(1),
		Repository:     repositoryObject(&login),
		Branch:         types.StringValue("main"),
		Build: types.ObjectValueMust(buildSettingsAttrTypes, map[string]attr.Value{
			"dockerfile": types.ObjectNull(dockerfileBuildAttrTypes),
			"buildpack": types.ObjectValueMust(buildpackBuildAttrTypes, map[string]attr.Value{
				"builder":       types.StringValue("HEROKU_24"),
				"build_context": types.StringValue("/web"),
				"buildpacks":    types.ListValueMust(types.StringType, nil),
				"use_cache":     types.BoolValue(false),
				"storage_size":  types.Int64Null(),
			}),
		}),
		BuildArguments:     types.MapNull(types.StringType),
		DisableCI:          types.BoolValue(true),
		Command:            types.StringValue("npm start"),
		Entrypoint:         types.StringNull(),
		Ports:              ports,
		HealthChecks:       checks,
		RuntimeEnvironment: types.MapValueMust(types.StringType, map[string]attr.Value{"PORT": types.StringValue("3000")}),
	}
}

func TestCombinedServicePayloadMatchesSpec(t *testing.T) {
	plan := combinedServicePlan(t)
	body, diags := combinedServiceRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	t.Run("create", func(t *testing.T) { roundTrip(t, body, &northflank.PostProjectsServicesCombinedJSONRequestBody{}) })
	t.Run("update", func(t *testing.T) { roundTrip(t, body, &northflank.PutProjectsServicesCombinedJSONRequestBody{}) })

	var payload map[string]any
	json.Unmarshal(body, &payload)

	if payload["vcsData"].(map[string]any)["projectBranch"] != "main" {
		t.Errorf("vcsData = %v, want the branch", payload["vcsData"])
	}

	// Branch and PR rules are build-service only and must not be sent.
	if config := payload["buildConfiguration"].(map[string]any); len(config) != 0 {
		t.Errorf("buildConfiguration = %v, want it empty for a combined service", config)
	}

	if billing := payload["billing"].(map[string]any); billing["deploymentPlan"] != "nf-compute-20" || billing["buildPlan"] != nil {
		t.Errorf("billing = %v, want the deployment plan and no build plan while unknown", billing)
	}
}

func TestApplyCombinedServiceRoundTrip(t *testing.T) {
	ctx := t.Context()
	plan := combinedServicePlan(t)
	body, _ := combinedServiceRequestBody(ctx, &plan)

	model := combinedServicePlan(t)

	if diags := applyCombinedService(ctx, &model, responseFor(t, serviceKindCombined, body)); diags.HasError() {
		t.Fatal(diags)
	}

	var repo repositoryModel
	model.Repository.As(ctx, &repo, objectAsOptions)

	// Configured, so the reported value is taken -- and shows as drift.
	if repo.AccountLogin.ValueString() != "picked-by-northflank" {
		t.Errorf("account_login = %v, want the reported value", repo.AccountLogin)
	}

	for name, pair := range map[string][2]attr.Value{
		"branch":              {model.Branch, plan.Branch},
		"build":               {model.Build, plan.Build},
		"command":             {model.Command, plan.Command},
		"ports":               {model.Ports, plan.Ports},
		"runtime_environment": {model.RuntimeEnvironment, plan.RuntimeEnvironment},
		"disable_ci":          {model.DisableCI, plan.DisableCI},
	} {
		if !pair[0].Equal(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
	}

	if !model.BuildPlan.IsNull() {
		t.Errorf("build_plan = %v, want null when the API reports none", model.BuildPlan)
	}
}

func TestServiceKindChecks(t *testing.T) {
	if err := checkServiceKind(&serviceRead{ID: "x", ServiceType: "build"}, serviceKindCombined); err == nil {
		t.Error("checkServiceKind() accepted a build service as combined")
	}

	if err := checkServiceKind(&serviceRead{ID: "x", ServiceType: "build"}, serviceKindBuild); err != nil {
		t.Errorf("checkServiceKind() = %v", err)
	}
}

func TestServiceImportIDs(t *testing.T) {
	ctx := t.Context()

	for _, newResource := range []func() fwresource.Resource{NewBuildServiceResource, NewCombinedServiceResource} {
		r := newResource()
		schemaResp := &fwresource.SchemaResponse{}
		r.Schema(ctx, fwresource.SchemaRequest{}, schemaResp)

		newState := func() tfsdk.State {
			return tfsdk.State{
				Schema: schemaResp.Schema,
				Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
			}
		}

		importer := r.(fwresource.ResourceWithImportState)

		resp := &fwresource.ImportStateResponse{State: newState()}
		importer.ImportState(ctx, fwresource.ImportStateRequest{ID: "my-project/svc"}, resp)

		if resp.Diagnostics.HasError() {
			t.Errorf("%T: ImportState() = %v", r, resp.Diagnostics)
		}

		resp = &fwresource.ImportStateResponse{State: newState()}
		importer.ImportState(ctx, fwresource.ImportStateRequest{ID: "svc"}, resp)

		if !resp.Diagnostics.HasError() {
			t.Errorf("%T: ImportState(svc) succeeded, want an error", r)
		}
	}
}
