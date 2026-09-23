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
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// blueprintTemplate is a template in the shape of a repository file, with a
// webhook token passed in as an argument.
const blueprintTemplate = `{
  "name": "PR Previews",
  "apiVersion": "v1.2",
  "description": "A preview per pull request",
  "arguments": {"WEBHOOK_TOKEN": ""},
  "options": {"autorun": true, "concurrencyPolicy": "latest"},
  "triggers": [
    {"kind": "webhook", "ref": "deploy-hook", "spec": {"token": "${args.WEBHOOK_TOKEN}"}}
  ],
  "spec": {"kind": "Workflow", "spec": {"type": "sequential", "steps": [{"kind": "Project", "spec": {"name": "pr-${refs.deploy-hook.branch}"}}]}}
}`

func blueprintPlan(t *testing.T) previewBlueprintModel {
	t.Helper()

	return previewBlueprintModel{
		ProjectID: types.StringValue("my-project"),
		Template:  types.StringValue(blueprintTemplate),
		ArgumentOverrides: types.MapValueMust(types.StringType, map[string]attr.Value{
			"WEBHOOK_TOKEN": types.StringValue("s3cret"),
		}),
	}
}

// The template must reach Northflank exactly as it is in the file, with the
// overrides added and nothing else touched.
func TestPreviewBlueprintBodyKeepsTemplate(t *testing.T) {
	plan := blueprintPlan(t)
	body, name, diags := previewBlueprintBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if name != "PR Previews" {
		t.Errorf("name = %q", name)
	}

	var sent, file map[string]any
	json.Unmarshal(body, &sent)
	json.Unmarshal([]byte(blueprintTemplate), &file)

	for key, value := range file {
		if !reflect.DeepEqual(sent[key], value) {
			t.Errorf("%s changed on the way out\n got: %v\nwant: %v", key, sent[key], value)
		}
	}

	if _, ok := sent["gitops"]; ok {
		t.Errorf("gitops = %v, want none", sent["gitops"])
	}

	if !reflect.DeepEqual(sent["argumentOverrides"], map[string]any{"WEBHOOK_TOKEN": "s3cret"}) {
		t.Errorf("argumentOverrides = %v", sent["argumentOverrides"])
	}

	// The token is only ever a reference in the template.
	if strings.Contains(blueprintTemplate, "s3cret") {
		t.Fatal("test template leaks the secret")
	}

	roundTrip(t, body, &northflank.PostProjectsPreviewBlueprintsJSONRequestBody{})
}

// A template that asks for GitOps itself still must not link the blueprint to
// the repository, since Northflank would then write to it.
func TestPreviewBlueprintBodyDropsGitOps(t *testing.T) {
	plan := blueprintPlan(t)
	plan.Template = types.StringValue(strings.Replace(blueprintTemplate, `"name": "PR Previews",`,
		`"name": "PR Previews", "gitops": {"repoUrl": "https://github.com/acme/app", "vcsService": "github", "branch": "main", "filePath": "/p.json"},`, 1))

	body, _, diags := previewBlueprintBody(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	var sent map[string]any
	json.Unmarshal(body, &sent)

	if _, ok := sent["gitops"]; ok {
		t.Errorf("gitops = %v, want it dropped", sent["gitops"])
	}
}

func TestParseBlueprintTemplate(t *testing.T) {
	if _, name, err := parseBlueprintTemplate(blueprintTemplate); err != nil || name != "PR Previews" {
		t.Fatalf("parseBlueprintTemplate() = %q, %v", name, err)
	}

	for label, raw := range map[string]string{
		"not JSON":        "name: PR Previews",
		"an array":        `[]`,
		"no apiVersion":   `{"name":"X","spec":{}}`,
		"no spec":         `{"name":"X","apiVersion":"v1.2"}`,
		"empty name":      `{"name":"","apiVersion":"v1.2","spec":{}}`,
		"spec not object": `{"name":"X","apiVersion":"v1.2","spec":[]}`,
	} {
		if _, _, err := parseBlueprintTemplate(raw); err == nil {
			t.Errorf("%s: accepted, want an error", label)
		}
	}
}

// A new name in the template means a new blueprint, since Northflank keys
// blueprints on it; other template changes are updates.
func TestPreviewBlueprintModifyPlan(t *testing.T) {
	ctx := t.Context()
	r := NewPreviewBlueprintResource().(*previewBlueprintResource)

	schemaResp := &fwresource.SchemaResponse{}
	r.Schema(ctx, fwresource.SchemaRequest{}, schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx)

	value := func(template, name string) tftypes.Value {
		attrs := map[string]tftypes.Value{}

		for key, typ := range objectType.(tftypes.Object).AttributeTypes {
			attrs[key] = tftypes.NewValue(typ, nil)
		}

		attrs["project_id"] = tftypes.NewValue(tftypes.String, "p")
		attrs["template"] = tftypes.NewValue(tftypes.String, template)

		if name == "?" {
			attrs["name"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		} else {
			attrs["name"] = tftypes.NewValue(tftypes.String, name)
		}

		return tftypes.NewValue(objectType, attrs)
	}

	renamed := strings.Replace(blueprintTemplate, `"PR Previews"`, `"PR Envs"`, 1)
	reworded := strings.Replace(blueprintTemplate, "A preview per pull request", "Previews", 1)

	for label, tt := range map[string]struct {
		template string
		replace  bool
	}{
		"rename":        {renamed, true},
		"other changes": {reworded, false},
	} {
		t.Run(label, func(t *testing.T) {
			plan := value(tt.template, "?")
			req := fwresource.ModifyPlanRequest{
				State: tfsdk.State{Schema: schemaResp.Schema, Raw: value(blueprintTemplate, "PR Previews")},
				Plan:  tfsdk.Plan{Schema: schemaResp.Schema, Raw: plan},
			}
			resp := &fwresource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: schemaResp.Schema, Raw: plan}}

			r.ModifyPlan(ctx, req, resp)

			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}

			var name types.String
			resp.Plan.GetAttribute(ctx, path.Root("name"), &name)

			wantName := "PR Previews"

			if tt.replace {
				wantName = "PR Envs"
			}

			if name.ValueString() != wantName {
				t.Errorf("planned name = %v, want %s", name, wantName)
			}

			if got := len(resp.RequiresReplace) > 0; got != tt.replace {
				t.Errorf("requires replace = %v, want %v", got, tt.replace)
			}
		})
	}
}

func TestPreviewBlueprintLookupAndRead(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/projects/p/preview-blueprints", func(w http.ResponseWriter, r *http.Request) {
		// The generated client only decodes JSON200 for a JSON content type.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"workflows":[
			{"id":"other","name":"Other","apiVersion":"v1.2"},
			{"id":"pr-previews","name":"PR Previews","apiVersion":"v1.2"}
		]},"pagination":{"hasNextPage":false,"count":2}}`)
	})

	mux.HandleFunc("GET /v1/projects/p/preview-blueprints/pr-previews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"name":"PR Previews","apiVersion":"v1.2","status":"success","paused":true,
			"spec":{"kind":"Workflow"}}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, _ := northflank.NewAuthenticated(server.URL, "tok")
	ctx := t.Context()

	id, err := findPreviewBlueprintID(ctx, client, "p", "PR Previews")

	if err != nil || id != "pr-previews" {
		t.Fatalf("findPreviewBlueprintID() = %q, %v", id, err)
	}

	read, err := getPreviewBlueprint(ctx, client, "p", id)

	if err != nil {
		t.Fatal(err)
	}

	plan := blueprintPlan(t)
	applyPreviewBlueprint(&plan, read)

	if plan.Template.ValueString() != blueprintTemplate || plan.Status.ValueString() != "success" || !plan.Paused.ValueBool() {
		t.Errorf("template kept %v, status %v, paused %v", plan.Template.ValueString() == blueprintTemplate, plan.Status, plan.Paused)
	}

	if idFromBody([]byte(`{"data":{"id":"x"}}`)) != "x" || idFromBody([]byte(`{}`)) != "" {
		t.Error("idFromBody is wrong")
	}
}
