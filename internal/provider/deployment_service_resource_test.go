package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// serviceResponse is a get-service response for a deployment service, in the
// shape the spec documents.
const serviceResponse = `{"data":{
	"id":"api","uid":"68f0aa1b9c4e2d7f11a3b402","appId":"/team/my-project/api","name":"API",
	"description":"Public API","tags":["backend"],"serviceType":"deployment",
	"createdAt":"2024-05-01T10:00:00.123Z","updatedAt":"2024-05-02T10:00:00Z",
	"billing":{"deploymentPlan":"nf-compute-20"},
	"cluster":{"id":"nf-europe-west","name":"nf-europe-west"},
	"status":{"deployment":{"status":"COMPLETED"}},
	"deployment":{
		"instances":2,
		"external":{"imagePath":"ghcr.io/acme/api:1.4"},
		"docker":{"configType":"customCommand","customCommand":"node server.js"}
	},
	"ports":[{"name":"http","internalPort":8080,"protocol":"HTTP","public":true,"domains":["api.example.com"]}],
	"healthChecks":[{"type":"readinessProbe","protocol":"HTTP","path":"/healthz","port":8080,
		"initialDelaySeconds":5,"periodSeconds":10,"timeoutSeconds":2,"failureThreshold":3,"successThreshold":1}],
	"runtimeEnvironment":{"LOG_LEVEL":"info"}
}}`

// The read struct is hand-written, so check that every key the test response
// uses is one the generated deployment variant -- which follows the spec --
// knows about: a misspelt key would silently read as zero.
func TestServiceReadMatchesSpec(t *testing.T) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal([]byte(serviceResponse), &envelope); err != nil {
		t.Fatal(err)
	}

	var generated northflank.GetProjectsServicesServiceid200JSONResponseBodyData2

	if err := json.Unmarshal(envelope.Data, &generated); err != nil {
		t.Fatalf("the generated type rejects the response: %v", err)
	}

	again, err := json.Marshal(generated)

	if err != nil {
		t.Fatal(err)
	}

	var want, got map[string]any
	json.Unmarshal(envelope.Data, &want)
	json.Unmarshal(again, &got)

	// Timestamps are re-encoded, so compare the key sets and the sections the
	// provider reads.
	for _, key := range []string{"deployment", "ports", "healthChecks", "runtimeEnvironment", "billing", "tags"} {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("%s changed in a round trip through the generated type\n got: %v\nwant: %v", key, got[key], want[key])
		}
	}

	service, err := serviceFromBody([]byte(serviceResponse))

	if err != nil {
		t.Fatalf("serviceFromBody() error = %v", err)
	}

	if service.Deployment.Instances != 2 || service.Deployment.External.ImagePath != "ghcr.io/acme/api:1.4" {
		t.Errorf("deployment = %+v", service.Deployment)
	}

	if service.CreatedAt == nil || service.CreatedAt.Nanosecond() != 123_000_000 {
		t.Errorf("createdAt = %v, want fractional seconds parsed", service.CreatedAt)
	}

	if *service.command() != "node server.js" || service.entrypoint() != nil {
		t.Errorf("command %v, entrypoint %v", service.command(), service.entrypoint())
	}
}

func servicePlan(t *testing.T) deploymentServiceModel {
	t.Helper()
	ctx := t.Context()
	command := "node server.js"

	disableNfDomain := true
	ports, diags := portsValue(ctx, []servicePortPayload{
		{Name: "http", InternalPort: 8080, Protocol: "HTTP", Public: true, Domains: []string{"api.example.com"},
			DisableNfDomain: &disableNfDomain},
	}, types.ListNull(types.ObjectType{AttrTypes: servicePortAttrTypes}))

	if diags.HasError() {
		t.Fatal(diags)
	}

	one := 1
	path, port := "/healthz", 8080
	checks, diags := healthChecksValue(ctx, []serviceHealthCheckPayload{{
		Type: "readinessProbe", Protocol: "HTTP", Path: &path, Port: &port,
		InitialDelaySeconds: 5, PeriodSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3, SuccessThreshold: &one,
	}}, types.ListNull(types.ObjectType{AttrTypes: serviceHealthCheckAttrTypes}))

	if diags.HasError() {
		t.Fatal(diags)
	}

	deployment := types.ObjectValueMust(deploymentSourceAttrTypes, map[string]attr.Value{
		"external_image": types.ObjectValueMust(externalImageAttrTypes, map[string]attr.Value{
			"image_path":     types.StringValue("ghcr.io/acme/api:1.4"),
			"credentials_id": types.StringValue("ghcr-creds"),
		}),
		"build_service": types.ObjectNull(buildServiceAttrTypes),
		"command":       types.StringValue(command),
		"entrypoint":    types.StringNull(),
	})

	return deploymentServiceModel{
		ProjectID:      types.StringValue("my-project"),
		Name:           types.StringValue("API"),
		Description:    types.StringValue("Public API"),
		Tags:           types.SetValueMust(types.StringType, stringValues([]string{"backend"})),
		DeploymentPlan: types.StringValue("nf-compute-20"),
		Instances:      types.Int64Value(2),
		Deployment:     deployment,
		Ports:          ports,
		HealthChecks:   checks,
		RuntimeEnvironment: types.MapValueMust(types.StringType, map[string]attr.Value{
			"LOG_LEVEL": types.StringValue("info"),
		}),
	}
}

func TestDeploymentServicePayloadMatchesSpec(t *testing.T) {
	plan := servicePlan(t)
	body, diags := deploymentServiceRequestBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	var want map[string]any

	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]any{
		"create": &northflank.PostProjectsServicesDeploymentJSONRequestBody{},
		"update": &northflank.PutProjectsServicesDeploymentJSONRequestBody{},
	} {
		t.Run(name, func(t *testing.T) {
			if err := json.Unmarshal(body, target); err != nil {
				t.Fatalf("the generated type rejects the payload: %v", err)
			}

			again, _ := json.Marshal(target)

			var got map[string]any
			json.Unmarshal(again, &got)

			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip through %T changed the payload\n got: %v\nwant: %v", target, got, want)
			}
		})
	}

	deployment := want["deployment"].(map[string]any)

	if deployment["instances"] != float64(2) || deployment["external"] == nil {
		t.Errorf("deployment = %v, want the instance count next to the source", deployment)
	}
}

func TestApplyDeploymentService(t *testing.T) {
	ctx := t.Context()
	service, err := serviceFromBody([]byte(serviceResponse))

	if err != nil {
		t.Fatal(err)
	}

	plan := servicePlan(t)
	want := servicePlan(t)

	if diags := applyDeploymentService(ctx, &plan, service); diags.HasError() {
		t.Fatal(diags)
	}

	// Reading back what was applied must not show a difference. Credentials
	// are not reported, and are kept from the configuration.
	for name, pair := range map[string][2]attr.Value{
		"deployment":          {plan.Deployment, want.Deployment},
		"ports":               {plan.Ports, want.Ports},
		"health_checks":       {plan.HealthChecks, want.HealthChecks},
		"runtime_environment": {plan.RuntimeEnvironment, want.RuntimeEnvironment},
		"instances":           {plan.Instances, want.Instances},
		"tags":                {plan.Tags, want.Tags},
	} {
		if !pair[0].Equal(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
	}

	if plan.CreatedAt.ValueString() != "2024-05-01T10:00:00Z" {
		t.Errorf("created_at = %v", plan.CreatedAt)
	}
}

// A service running "latest" must not start showing the commit Northflank
// resolved it to; any other change of source is drift and must show.
func TestServiceDeploymentValueBuildService(t *testing.T) {
	ctx := t.Context()

	configured := types.ObjectValueMust(deploymentSourceAttrTypes, map[string]attr.Value{
		"external_image": types.ObjectNull(externalImageAttrTypes),
		"build_service": types.ObjectValueMust(buildServiceAttrTypes, map[string]attr.Value{
			"id": types.StringValue("builder"), "branch": types.StringValue("main"), "build_sha": types.StringValue("latest"),
		}),
		"command":    types.StringNull(),
		"entrypoint": types.StringNull(),
	})

	var service serviceRead
	json.Unmarshal([]byte(`{"serviceType":"deployment","deployment":{"instances":1,
		"internal":{"id":"builder","branch":"main","buildSHA":"0123456789abcdef0123456789abcdef01234567"}}}`), &service)

	value, diags := serviceDeploymentValue(ctx, configured, &service)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if !value.Equal(configured) {
		t.Errorf("deployment = %v, want the configured latest kept", value)
	}

	// The service was switched to an external image outside Terraform.
	service = serviceRead{}
	json.Unmarshal([]byte(`{"serviceType":"deployment","deployment":{"instances":1,
		"external":{"imagePath":"nginx:latest"}}}`), &service)

	value, _ = serviceDeploymentValue(ctx, configured, &service)

	var got deploymentSourceModel
	value.As(ctx, &got, objectAsOptions)

	if !got.BuildService.IsNull() || got.ExternalImage.IsNull() {
		t.Errorf("deployment = %v, want the drift to an external image reported", value)
	}
}

func TestDeploymentServiceImportState(t *testing.T) {
	ctx := t.Context()
	r := NewDeploymentServiceResource().(*deploymentServiceResource)

	schemaResp := &fwresource.SchemaResponse{}
	r.Schema(ctx, fwresource.SchemaRequest{}, schemaResp)

	newState := func() tfsdk.State {
		return tfsdk.State{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
		}
	}

	resp := &fwresource.ImportStateResponse{State: newState()}
	r.ImportState(ctx, fwresource.ImportStateRequest{ID: "my-project/api"}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}

	for _, bad := range []string{"api", "my-project/", "a/b/c"} {
		resp := &fwresource.ImportStateResponse{State: newState()}
		r.ImportState(ctx, fwresource.ImportStateRequest{ID: bad}, resp)

		if !resp.Diagnostics.HasError() {
			t.Errorf("ImportState(%q) succeeded, want an error", bad)
		}
	}
}

func TestServiceDataSourceLookups(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/projects/my-project/services", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"services":[
			{"id":"api","uid":"u","appId":"a","name":"API","projectId":"my-project","serviceType":"deployment",
			 "disabledCI":false,"disabledCD":false,"tags":[],"status":{}}
		]},"pagination":{"hasNextPage":false,"count":1}}`)
	})

	mux.HandleFunc("GET /v1/projects/my-project/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "api" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, serviceResponse)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := northflank.NewAuthenticated(server.URL, "tok")

	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	d := &serviceDataSource{client: client}

	id, err := d.findServiceIDByName(ctx, "my-project", "API")

	if err != nil || id != "api" {
		t.Fatalf("findServiceIDByName() = %q, %v", id, err)
	}

	service, err := getService(ctx, client, "my-project", id)

	if err != nil {
		t.Fatalf("getService() error = %v", err)
	}

	model, diags := serviceDataSourceModelFrom(ctx, "my-project", service)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if model.ServiceType.ValueString() != "deployment" || model.ImagePath.ValueString() != "ghcr.io/acme/api:1.4" ||
		model.Instances.ValueInt64() != 2 || !model.BuildServiceID.IsNull() || len(model.Ports.Elements()) != 1 {
		t.Errorf("model = %+v", model)
	}

	if _, err := getService(ctx, client, "my-project", "gone"); !isNotFound(err) {
		t.Errorf("error = %v, want a 404", err)
	}
}
