package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func externalImageDeployment(t *testing.T, command *string) types.Object {
	t.Helper()

	image := types.ObjectValueMust(externalImageAttrTypes, map[string]attr.Value{
		"image_path":     types.StringValue("ghcr.io/acme/report:1.2"),
		"credentials_id": types.StringValue("ghcr-creds"),
	})

	return types.ObjectValueMust(deploymentSourceAttrTypes, map[string]attr.Value{
		"external_image": image,
		"build_service":  types.ObjectNull(buildServiceAttrTypes),
		"command":        types.StringPointerValue(command),
		"entrypoint":     types.StringNull(),
	})
}

func jobPlan(t *testing.T) jobResourceModel {
	t.Helper()
	command := "node report.js"

	return jobResourceModel{
		ProjectID:             types.StringValue("my-project"),
		Name:                  types.StringValue("Nightly Report"),
		Description:           types.StringValue("Builds the nightly report"),
		Tags:                  types.SetValueMust(types.StringType, stringValues([]string{"reports"})),
		DeploymentPlan:        types.StringValue("nf-compute-20"),
		Deployment:            externalImageDeployment(t, &command),
		BackoffLimit:          types.Int64Value(2),
		ActiveDeadlineSeconds: types.Int64Value(3600),
		Cron: types.ObjectValueMust(jobCronAttrTypes, map[string]attr.Value{
			"schedule":           types.StringValue("30 2 * * *"),
			"concurrency_policy": types.StringValue("forbid"),
		}),
		RuntimeEnvironment: types.MapValueMust(types.StringType, map[string]attr.Value{
			"REPORT_BUCKET": types.StringValue("s3://reports"),
		}),
	}
}

// The payload is hand-written, so check that the generated create and update
// types -- which follow the spec -- keep every field of it.
func TestJobPayloadMatchesSpec(t *testing.T) {
	plan := jobPlan(t)
	body, diags := jobRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	var want map[string]any

	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]any{
		"create": &northflank.PostProjectsJobsJSONRequestBody{},
		"update": &northflank.PutProjectsJobsJSONRequestBody{},
	} {
		t.Run(name, func(t *testing.T) {
			if err := json.Unmarshal(body, target); err != nil {
				t.Fatalf("the generated type rejects the payload: %v", err)
			}

			again, err := json.Marshal(target)

			if err != nil {
				t.Fatal(err)
			}

			var got map[string]any

			if err := json.Unmarshal(again, &got); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip through %T changed the payload\n got: %v\nwant: %v", target, got, want)
			}
		})
	}
}

func TestJobPayloadContents(t *testing.T) {
	plan := jobPlan(t)
	body, diags := jobRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	want := `{"name":"Nightly Report","description":"Builds the nightly report","tags":["reports"],` +
		`"billing":{"deploymentPlan":"nf-compute-20"},` +
		`"deployment":{"external":{"imagePath":"ghcr.io/acme/report:1.2","credentials":"ghcr-creds"},` +
		`"docker":{"configType":"customCommand","customCommand":"node report.js"}},` +
		`"settings":{"backoffLimit":2,"activeDeadlineSeconds":3600,"cron":{"schedule":"30 2 * * *","concurrencyPolicy":"forbid"}},` +
		`"runtimeEnvironment":{"REPORT_BUCKET":"s3://reports"}}`

	if string(body) != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}

	t.Run("manual job from a build service", func(t *testing.T) {
		plan.Cron = types.ObjectNull(jobCronAttrTypes)
		plan.ActiveDeadlineSeconds = types.Int64Unknown()
		plan.RuntimeEnvironment = types.MapNull(types.StringType)
		plan.Deployment = types.ObjectValueMust(deploymentSourceAttrTypes, map[string]attr.Value{
			"external_image": types.ObjectNull(externalImageAttrTypes),
			"build_service": types.ObjectValueMust(buildServiceAttrTypes, map[string]attr.Value{
				"id":        types.StringValue("builder"),
				"branch":    types.StringValue("main"),
				"build_sha": types.StringValue("latest"),
			}),
			"command":    types.StringNull(),
			"entrypoint": types.StringNull(),
		})

		payload, diags := jobPayloadFrom(t.Context(), &plan)

		if diags.HasError() {
			t.Fatal(diags)
		}

		got, _ := json.Marshal(payload.Deployment)

		if string(got) != `{"internal":{"id":"builder","branch":"main","buildSHA":"latest"},"docker":{"configType":"default"}}` {
			t.Errorf("deployment = %s", got)
		}

		settings, _ := json.Marshal(payload.Settings)

		if string(settings) != `{"backoffLimit":2}` {
			t.Errorf("settings = %s, want no cron and no deadline", settings)
		}

		if payload.RuntimeEnvironment == nil || len(payload.RuntimeEnvironment) != 0 {
			t.Errorf("runtimeEnvironment = %v, want an explicit empty map", payload.RuntimeEnvironment)
		}
	})
}

func TestDockerOverride(t *testing.T) {
	command, entrypoint := "run", "/bin/sh"

	tests := []struct {
		name       string
		command    *string
		entrypoint *string
		want       string
	}{
		{"neither", nil, nil, "default"},
		{"command", &command, nil, "customCommand"},
		{"entrypoint", nil, &entrypoint, "customEntrypoint"},
		{"both", &command, &entrypoint, "customEntrypointCustomCommand"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dockerOverride(tt.command, tt.entrypoint)

			if got.ConfigType != tt.want {
				t.Errorf("configType = %q, want %q", got.ConfigType, tt.want)
			}

			// Round-trip through the read side, which must invert it.
			view, _ := jobFromGet(fakeJobGet(t, got))

			if !reflect.DeepEqual(view.Command, tt.command) || !reflect.DeepEqual(view.Entrypoint, tt.entrypoint) {
				t.Errorf("read back command %v, entrypoint %v", view.Command, view.Entrypoint)
			}
		})
	}
}

// fakeJobGet builds a GET response carrying the given docker override.
func fakeJobGet(t *testing.T, docker *dockerPayload) *northflank.GetProjectsJobsJobidHTTPResponse {
	t.Helper()

	dockerJSON, _ := json.Marshal(docker)
	body := fmt.Sprintf(`{"data":{"id":"j","uid":"u","appId":"/a/p/j","name":"J","tags":[],"projectId":"p",
		"jobType":"manual","createdAt":"2024-05-01T00:00:00Z","disabledCI":false,"disabledCD":false,
		"billing":{"deploymentPlan":"nf-compute-20"},"settings":{"backoffLimit":0,"activeDeadlineSeconds":0},
		"cluster":{"id":"c","name":"c"},"deployment":{"docker":%s}}}`, dockerJSON)

	var resp northflank.GetProjectsJobsJobidHTTPResponse

	if err := json.Unmarshal([]byte(body), &resp.JSON200); err != nil {
		t.Fatal(err)
	}

	return &resp
}

func TestApplyJobKeepsUnreportedValues(t *testing.T) {
	ctx := t.Context()
	plan := jobPlan(t)
	env := plan.RuntimeEnvironment

	diags := applyJob(ctx, &plan, &jobView{
		ID:             "nightly-report",
		ProjectID:      "my-project",
		Name:           "Nightly Report",
		JobType:        "manual",
		DeploymentPlan: "nf-compute-20",
		BackoffLimit:   2,
		// The command was removed outside Terraform.
	})

	if diags.HasError() {
		t.Fatal(diags)
	}

	var deployment deploymentSourceModel
	plan.Deployment.As(ctx, &deployment, objectAsOptions)

	var image externalImageModel
	deployment.ExternalImage.As(ctx, &image, objectAsOptions)

	if image.ImagePath.ValueString() != "ghcr.io/acme/report:1.2" {
		t.Errorf("image_path = %v, want the configured image kept", image.ImagePath)
	}

	if !deployment.Command.IsNull() {
		t.Errorf("command = %v, want the drift reported as null", deployment.Command)
	}

	if !plan.RuntimeEnvironment.Equal(env) {
		t.Errorf("runtime_environment = %v, want it kept", plan.RuntimeEnvironment)
	}

	if !plan.Cron.IsNull() || !plan.ActiveDeadlineSeconds.IsNull() {
		t.Errorf("cron %v, active_deadline_seconds %v; want both null", plan.Cron, plan.ActiveDeadlineSeconds)
	}
}

// Adding or removing the cron block changes the job type, which forces a new
// job; changing the schedule does not.
func TestJobCronRequiresReplace(t *testing.T) {
	ctx := t.Context()
	resp := &fwresource.SchemaResponse{}
	NewJobResource().Schema(ctx, fwresource.SchemaRequest{}, resp)

	modifiers := resp.Schema.Attributes["cron"].(interface {
		ObjectPlanModifiers() []planmodifier.Object
	}).ObjectPlanModifiers()

	cron := func(schedule string) types.Object {
		return types.ObjectValueMust(jobCronAttrTypes, map[string]attr.Value{
			"schedule":           types.StringValue(schedule),
			"concurrency_policy": types.StringValue("forbid"),
		})
	}

	null := types.ObjectNull(jobCronAttrTypes)

	tests := []struct {
		name        string
		state, plan types.Object
		want        bool
	}{
		{"manual to cron", null, cron("@daily"), true},
		{"cron to manual", cron("@daily"), null, true},
		{"new schedule", cron("@daily"), cron("@hourly"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := planmodifier.ObjectRequest{
				StateValue:  tt.state,
				PlanValue:   tt.plan,
				ConfigValue: tt.plan,
				// Non-null raw state and plan: an update, not a create or destroy.
				State: tfsdk.State{Raw: tftypes.NewValue(tftypes.Object{}, map[string]tftypes.Value{})},
				Plan:  tfsdk.Plan{Raw: tftypes.NewValue(tftypes.Object{}, map[string]tftypes.Value{})},
			}
			out := &planmodifier.ObjectResponse{PlanValue: tt.plan}

			for _, m := range modifiers {
				m.PlanModifyObject(ctx, req, out)
			}

			if out.RequiresReplace != tt.want {
				t.Errorf("RequiresReplace = %v, want %v", out.RequiresReplace, tt.want)
			}
		})
	}
}

func TestJobImportState(t *testing.T) {
	ctx := t.Context()
	r := NewJobResource().(*jobResource)

	schemaResp := &fwresource.SchemaResponse{}
	r.Schema(ctx, fwresource.SchemaRequest{}, schemaResp)

	newState := func() tfsdk.State {
		return tfsdk.State{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
		}
	}

	resp := &fwresource.ImportStateResponse{State: newState()}
	r.ImportState(ctx, fwresource.ImportStateRequest{ID: "my-project/nightly-report"}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}

	var id types.String
	resp.State.GetAttribute(ctx, pathRoot("id"), &id)

	if id.ValueString() != "nightly-report" {
		t.Errorf("id = %v", id)
	}

	for _, bad := range []string{"nightly-report", "my-project/", "a/b/c"} {
		resp := &fwresource.ImportStateResponse{State: newState()}
		r.ImportState(ctx, fwresource.ImportStateRequest{ID: bad}, resp)

		if !resp.Diagnostics.HasError() {
			t.Errorf("ImportState(%q) succeeded, want an error", bad)
		}
	}
}

func TestJobPatterns(t *testing.T) {
	tests := []struct {
		name  string
		match func(string) bool
		value string
		want  bool
	}{
		{"same-project build service", buildServiceIDPattern.MatchString, "builder", true},
		{"cross-project build service", buildServiceIDPattern.MatchString, "other-project/builder", true},
		{"build service with two slashes", buildServiceIDPattern.MatchString, "a/b/c", false},
		{"latest", buildSHAPattern.MatchString, "latest", true},
		{"full sha", buildSHAPattern.MatchString, strings.Repeat("a1", 20), true},
		{"short sha", buildSHAPattern.MatchString, "abc123", false},
		{"plan", jobPlanPattern.MatchString, "nf-compute-20", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.match(tt.value); got != tt.want {
				t.Errorf("match(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestJobDataSourceLookups(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/projects/my-project/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"jobs":[
			{"id":"nightly-report","uid":"u","appId":"a","name":"Nightly Report","projectId":"my-project",
			 "jobType":"cron","disabledCI":false,"disabledCD":false,"tags":[]}
		]},"pagination":{"hasNextPage":false,"count":1}}`)
	})

	mux.HandleFunc("GET /v1/projects/my-project/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "nightly-report" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"id":"nightly-report","uid":"u","appId":"/t/my-project/nightly-report",
			"name":"Nightly Report","tags":["reports"],"projectId":"my-project","jobType":"cron",
			"createdAt":"2024-05-01T00:00:00Z","disabledCI":false,"disabledCD":false,"suspended":true,
			"billing":{"deploymentPlan":"nf-compute-20"},
			"settings":{"backoffLimit":2,"activeDeadlineSeconds":3600,"cron":{"schedule":"30 2 * * *"}},
			"cluster":{"id":"c","name":"c"},
			"deployment":{"docker":{"configType":"customCommand","customCommand":"node report.js"}}}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := northflank.NewAuthenticated(server.URL, "tok")

	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	d := &jobDataSource{client: client}

	id, err := d.findJobIDByName(ctx, "my-project", "Nightly Report")

	if err != nil || id != "nightly-report" {
		t.Fatalf("findJobIDByName() = %q, %v", id, err)
	}

	job, err := getJob(ctx, client, "my-project", id)

	if err != nil {
		t.Fatalf("getJob() error = %v", err)
	}

	model, diags := jobDataSourceModelFrom(ctx, job)

	if diags.HasError() {
		t.Fatal(diags)
	}

	var cron jobCronModel
	model.Cron.As(ctx, &cron, objectAsOptions)

	checks := map[string][2]any{
		"command":            {model.Command.ValueString(), "node report.js"},
		"schedule":           {cron.Schedule.ValueString(), "30 2 * * *"},
		"concurrency_policy": {cron.ConcurrencyPolicy.ValueString(), "forbid"},
		"suspended":          {model.Suspended.ValueBool(), true},
		"deadline":           {model.ActiveDeadlineSeconds.ValueInt64(), int64(3600)},
		"entrypoint_null":    {model.Entrypoint.IsNull(), true},
	}

	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}

	if _, err := getJob(ctx, client, "my-project", "gone"); !isNotFound(err) {
		t.Errorf("error = %v, want a 404", err)
	}
}
