package provider

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// gitJobPlan is a job that builds from Git, with a command override and no
// image or build service.
func gitJobPlan(t *testing.T) jobResourceModel {
	t.Helper()
	target := "worker"

	plan := jobPlan(t)
	plan.Deployment = types.ObjectValueMust(deploymentSourceAttrTypes, map[string]attr.Value{
		"external_image": types.ObjectNull(externalImageAttrTypes),
		"build_service":  types.ObjectNull(buildServiceAttrTypes),
		"command":        types.StringValue("node report.js"),
		"entrypoint":     types.StringNull(),
	})
	plan.Repository = repositoryObject(nil)
	plan.Branch = types.StringValue("main")
	plan.Build = dockerfileBuild("buildkit", &target)
	plan.BuildPlan = types.StringValue("nf-compute-200")
	plan.DisableCI = types.BoolValue(false)
	plan.BuildArguments = types.MapValueMust(types.StringType, map[string]attr.Value{
		"NPM_TOKEN": types.StringValue("secret"),
	})

	return plan
}

func TestJobGitPayloadMatchesSpec(t *testing.T) {
	plan := gitJobPlan(t)
	body, diags := jobRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	t.Run("create", func(t *testing.T) { roundTrip(t, body, &northflank.PostProjectsJobsJSONRequestBody{}) })
	t.Run("update", func(t *testing.T) { roundTrip(t, body, &northflank.PutProjectsJobsJSONRequestBody{}) })

	var payload map[string]any
	json.Unmarshal(body, &payload)

	deployment := payload["deployment"].(map[string]any)

	wantVCS := map[string]any{"projectUrl": "https://github.com/acme/api", "projectType": "github", "projectBranch": "main"}

	if !reflect.DeepEqual(deployment["vcs"], wantVCS) {
		t.Errorf("deployment.vcs = %v, want %v", deployment["vcs"], wantVCS)
	}

	if deployment["external"] != nil || deployment["internal"] != nil {
		t.Errorf("deployment = %v, want the repository as the only source", deployment)
	}

	if deployment["docker"].(map[string]any)["customCommand"] != "node report.js" {
		t.Errorf("docker = %v, want the command override kept", deployment["docker"])
	}

	if payload["billing"].(map[string]any)["buildPlan"] != "nf-compute-200" {
		t.Errorf("billing = %v", payload["billing"])
	}

	if payload["buildConfiguration"].(map[string]any)["dockerfileTarget"] != "worker" || payload["buildSettings"] == nil {
		t.Errorf("build settings = %v / %v", payload["buildSettings"], payload["buildConfiguration"])
	}

	if payload["buildArguments"].(map[string]any)["NPM_TOKEN"] != "secret" {
		t.Errorf("buildArguments = %v", payload["buildArguments"])
	}
}

// A job without Git sends none of the build fields.
func TestJobImagePayloadHasNoBuild(t *testing.T) {
	plan := jobPlan(t)
	body, _ := jobRequestBody(t.Context(), &plan)

	var payload map[string]any
	json.Unmarshal(body, &payload)

	for _, key := range []string{"buildSettings", "buildConfiguration", "buildArguments", "disabledCI"} {
		if _, ok := payload[key]; ok {
			t.Errorf("%s = %v, want it left out", key, payload[key])
		}
	}

	if _, ok := payload["billing"].(map[string]any)["buildPlan"]; ok {
		t.Error("billing.buildPlan sent for a job without Git")
	}
}

// A job that builds from Git may leave the deployment block out altogether.
func TestJobPayloadWithoutDeploymentBlock(t *testing.T) {
	plan := gitJobPlan(t)
	plan.Deployment = types.ObjectNull(deploymentSourceAttrTypes)

	payload, diags := jobPayloadFrom(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if payload.Deployment.VCS == nil || payload.Deployment.Docker.ConfigType != "default" {
		t.Errorf("deployment = %+v, want the repository and the image defaults", payload.Deployment)
	}
}

// What was applied must read back without a difference.
func TestApplyJobBuildFromGit(t *testing.T) {
	ctx := t.Context()
	plan := gitJobPlan(t)
	body, _ := jobRequestBody(ctx, &plan)

	var sent map[string]any
	json.Unmarshal(body, &sent)

	// Echo the job back as the API reports it: the repository comes back as
	// vcsData, with the Git account Northflank picked filled in.
	vcs := sent["deployment"].(map[string]any)["vcs"].(map[string]any)
	vcs["accountLogin"] = "picked-by-northflank"
	settings, _ := json.Marshal(sent["buildSettings"])
	config, _ := json.Marshal(sent["buildConfiguration"])
	vcsJSON, _ := json.Marshal(vcs)

	response := fmt.Sprintf(`{"data":{"id":"nightly-report","uid":"u","appId":"a","name":"Nightly Report","tags":[],
		"projectId":"my-project","jobType":"cron","createdAt":"2024-05-01T00:00:00Z","disabledCI":false,"disabledCD":false,
		"billing":{"deploymentPlan":"nf-compute-20"},"settings":{"backoffLimit":2,"activeDeadlineSeconds":3600,
		"cron":{"schedule":"30 2 * * *","concurrencyPolicy":"forbid"}},"cluster":{"id":"c","name":"c"},
		"deployment":{"docker":{"configType":"customCommand","customCommand":"node report.js"}},
		"vcsData":%s,"buildSettings":%s,"buildConfiguration":%s}}`, vcsJSON, settings, config)

	var resp northflank.GetProjectsJobsJobidHTTPResponse
	resp.Body = []byte(response)

	if err := json.Unmarshal(resp.Body, &resp.JSON200); err != nil {
		t.Fatal(err)
	}

	job, err := jobFromGet(&resp)

	if err != nil {
		t.Fatal(err)
	}

	job.Build = jobBuildFromBody(resp.Body)

	if job.Build == nil {
		t.Fatal("jobBuildFromBody() = nil for a job with vcsData")
	}

	if jobBuildFromBody([]byte(`{"data":{"id":"j"}}`)) != nil {
		t.Error("jobBuildFromBody() found a build in a job without vcsData")
	}

	model := gitJobPlan(t)

	if diags := applyJob(ctx, &model, job); diags.HasError() {
		t.Fatal(diags)
	}

	for name, pair := range map[string][2]attr.Value{
		"repository":      {model.Repository, plan.Repository},
		"branch":          {model.Branch, plan.Branch},
		"build":           {model.Build, plan.Build},
		"build_arguments": {model.BuildArguments, plan.BuildArguments},
		"build_plan":      {model.BuildPlan, plan.BuildPlan},
		"deployment":      {model.Deployment, plan.Deployment},
	} {
		if !pair[0].Equal(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
	}
}

// A job without Git reads back with no repository, whatever state held.
func TestApplyJobWithoutGit(t *testing.T) {
	model := jobPlan(t)
	model.Repository = types.ObjectUnknown(repositoryAttrTypes)
	model.Branch = types.StringUnknown()

	applyJob(t.Context(), &model, &jobView{ID: "j", Name: "J"})

	if !model.Repository.IsNull() || !model.Branch.IsNull() || !model.Build.IsNull() {
		t.Errorf("repository %v, branch %v, build %v; want all null", model.Repository, model.Branch, model.Build)
	}
}

func TestJobDataSourceReportsRepository(t *testing.T) {
	ctx := t.Context()
	branch := "main"

	build := &serviceRead{}
	build.VcsData = &struct {
		ProjectURL      string  `json:"projectUrl"`
		ProjectType     string  `json:"projectType"`
		ProjectBranch   *string `json:"projectBranch"`
		AccountLogin    *string `json:"accountLogin"`
		VcsLinkID       *string `json:"vcsLinkId"`
		SelfHostedVcsID *string `json:"selfHostedVcsId"`
	}{ProjectURL: "https://github.com/acme/tools", ProjectType: "github", ProjectBranch: &branch}

	model, diags := jobDataSourceModelFrom(ctx, &jobView{ID: "cleanup", Name: "Nightly Cleanup", Build: build})

	if diags.HasError() {
		t.Fatal(diags)
	}

	if model.RepositoryURL.ValueString() != "https://github.com/acme/tools" || model.Branch.ValueString() != "main" {
		t.Errorf("repository_url %v, branch %v", model.RepositoryURL, model.Branch)
	}

	model, _ = jobDataSourceModelFrom(ctx, &jobView{ID: "report", Name: "Report"})

	if !model.RepositoryURL.IsNull() || !model.Branch.IsNull() {
		t.Errorf("repository_url %v, branch %v; want null for a job without Git", model.RepositoryURL, model.Branch)
	}
}

func TestBuildStoragePayload(t *testing.T) {
	ctx := t.Context()
	settings, _, diags := buildSettingsPayloadFrom(ctx, dockerfileBuild("buildkit", nil))

	if diags.HasError() {
		t.Fatal(diags)
	}

	raw, _ := json.Marshal(settings.Dockerfile.Storage)

	if string(raw) != `{"ephemeralStorage":{"storageSize":16384}}` {
		t.Errorf("storage = %s", raw)
	}

	if buildStorage(types.Int64Null()) != nil || buildStorage(types.Int64Unknown()) != nil {
		t.Error("an unset storage size was sent")
	}
}

// Northflank reporting a size wins; not reporting one keeps what was set, so
// that an apply does not come back inconsistent.
func TestStorageSizeValue(t *testing.T) {
	size := int64(32768)
	reported := &buildStoragePayload{}
	reported.EphemeralStorage = &struct {
		StorageSize *int64 `json:"storageSize,omitempty"`
	}{StorageSize: &size}

	if got := storageSizeValue(reported, types.Int64Value(16384)); got.ValueInt64() != 32768 {
		t.Errorf("reported size: got %v", got)
	}

	if got := storageSizeValue(nil, types.Int64Value(16384)); got.ValueInt64() != 16384 {
		t.Errorf("unreported size: got %v, want the prior value", got)
	}

	if got := storageSizeValue(nil, types.Int64Unknown()); !got.IsNull() {
		t.Errorf("unreported, unknown size: got %v, want null", got)
	}
}

func TestJobRunOnSourceChange(t *testing.T) {
	ctx := t.Context()

	plan := gitJobPlan(t)
	plan.RunOnSourceChange = types.StringValue("always")
	payload, diags := jobPayloadFrom(ctx, &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if payload.Settings.RunOnSourceChange != "always" {
		t.Errorf("runOnSourceChange = %q", payload.Settings.RunOnSourceChange)
	}

	body, _ := jobRequestBody(ctx, &plan)
	roundTrip(t, body, &northflank.PutProjectsJobsJSONRequestBody{})

	// Not reported by the API: kept when known, the default after an import.
	applyJob(ctx, &plan, &jobView{ID: "j", Name: "J"})

	if plan.RunOnSourceChange.ValueString() != "always" {
		t.Errorf("run_on_source_change = %v, want it kept", plan.RunOnSourceChange)
	}

	imported := jobResourceModel{RunOnSourceChange: types.StringNull()}
	applyJob(ctx, &imported, &jobView{ID: "j", Name: "J"})

	if imported.RunOnSourceChange.ValueString() != "never" {
		t.Errorf("imported run_on_source_change = %v, want never", imported.RunOnSourceChange)
	}
}
