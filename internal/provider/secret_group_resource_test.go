package provider

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func restrictionsObject(t *testing.T, restricted bool, resources []secretGroupObjectModel, tags []string, tmc *string) types.Object {
	t.Helper()
	ctx := t.Context()

	resourceSet := types.SetNull(types.ObjectType{AttrTypes: secretGroupObjectAttrTypes})

	if resources != nil {
		var d interface{ HasError() bool }
		resourceSet, d = types.SetValueFrom(ctx, types.ObjectType{AttrTypes: secretGroupObjectAttrTypes}, resources)

		if d.HasError() {
			t.Fatal(d)
		}
	}

	tagSet := types.SetNull(types.StringType)

	if tags != nil {
		tagSet = types.SetValueMust(types.StringType, stringValues(tags))
	}

	value, diags := types.ObjectValueFrom(ctx, secretGroupRestrictionsAttrTypes, secretGroupRestrictionsModel{
		Restricted:        types.BoolValue(restricted),
		Resources:         resourceSet,
		Tags:              tagSet,
		TagMatchCondition: types.StringPointerValue(tmc),
		StageIDs:          types.SetNull(types.StringType),
	})

	if diags.HasError() {
		t.Fatal(diags)
	}

	return value
}

func stringValues(values []string) []attr.Value {
	out := make([]attr.Value, len(values))

	for i, v := range values {
		out[i] = types.StringValue(v)
	}

	return out
}

func planModel(t *testing.T) secretGroupResourceModel {
	t.Helper()

	return secretGroupResourceModel{
		ProjectID:   types.StringValue("my-project"),
		Name:        types.StringValue("App Secrets"),
		Description: types.StringUnknown(),
		Priority:    types.Int64Value(20),
		SecretType:  types.StringValue("environment"),
		Type:        types.StringValue("secret"),
		Tags:        types.SetValueMust(types.StringType, stringValues([]string{"prod", "api"})),
		Variables: types.MapValueMust(types.StringType, map[string]attr.Value{
			"DATABASE_URL": types.StringValue("postgres://x"),
		}),
		Restrictions: restrictionsObject(t, true,
			[]secretGroupObjectModel{{ID: types.StringValue("api"), Type: types.StringValue("service")}},
			[]string{"backend"}, nil),
	}
}

// The payload is hand-written, so check that the generated request types --
// which follow the spec -- accept every field of it for both create and
// update. A field the spec does not know would be dropped on the round trip.
func TestSecretGroupPayloadMatchesSpec(t *testing.T) {
	plan := planModel(t)
	payload, diags := secretGroupPayloadFrom(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	payload.Name = plan.Name.ValueString()

	raw, err := json.Marshal(payload)

	if err != nil {
		t.Fatal(err)
	}

	var want map[string]any

	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	check := func(t *testing.T, target any, want map[string]any) {
		t.Helper()

		if err := json.Unmarshal(raw, target); err != nil {
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
	}

	t.Run("create", func(t *testing.T) {
		check(t, &northflank.PostProjectsSecretsJSONRequestBody{}, want)
	})

	t.Run("update", func(t *testing.T) {
		// The update body has no name.
		withoutName := map[string]any{}

		for k, v := range want {
			if k != "name" {
				withoutName[k] = v
			}
		}

		check(t, &northflank.PatchProjectsSecretsSecretidJSONRequestBody{}, withoutName)
	})
}

func TestSecretGroupPayloadContents(t *testing.T) {
	plan := planModel(t)
	payload, diags := secretGroupPayloadFrom(t.Context(), &plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if payload.Description != nil {
		t.Errorf("description = %q, want it omitted while unknown", *payload.Description)
	}

	if *payload.Priority != 20 || *payload.SecretType != "environment" || *payload.Type != "secret" {
		t.Errorf("scalars = %d %q %q", *payload.Priority, *payload.SecretType, *payload.Type)
	}

	if want := []string{"api", "prod"}; !reflect.DeepEqual(*payload.Tags, want) {
		t.Errorf("tags = %v, want %v", *payload.Tags, want)
	}

	r := payload.Restrictions

	if !r.Restricted || len(r.NfObjects) != 1 || r.NfObjects[0] != (secretGroupObjectPayload{ID: "api", Type: "service"}) {
		t.Errorf("restrictions = %+v", r)
	}

	if !reflect.DeepEqual(r.StageIds, []string{}) {
		t.Errorf("stageIds = %#v, want an explicit empty list", r.StageIds)
	}

	t.Run("no variables sends an empty map", func(t *testing.T) {
		plan.Variables = types.MapNull(types.StringType)
		payload, _ := secretGroupPayloadFrom(t.Context(), &plan)
		raw, _ := json.Marshal(payload.Secrets)

		if string(raw) != `{"variables":{}}` {
			t.Errorf("secrets = %s, want an explicit empty variables map", raw)
		}
	})

	t.Run("no restrictions lifts them", func(t *testing.T) {
		plan.Restrictions = types.ObjectNull(secretGroupRestrictionsAttrTypes)
		payload, _ := secretGroupPayloadFrom(t.Context(), &plan)
		raw, _ := json.Marshal(payload.Restrictions)

		if string(raw) != `{"restricted":false,"nfObjects":[],"tags":[],"stageIds":[]}` {
			t.Errorf("restrictions = %s", raw)
		}
	})
}

func TestApplySecretGroup(t *testing.T) {
	ctx := t.Context()
	description := "secrets for the app"
	or := "or"

	model := secretGroupResourceModel{Restrictions: types.ObjectNull(secretGroupRestrictionsAttrTypes)}

	diags := applySecretGroup(ctx, &model, &secretGroupView{
		ID:          "app-secrets",
		ProjectID:   "my-project",
		Name:        "App Secrets",
		Description: &description,
		Priority:    10,
		SecretType:  "environment-arguments",
		Type:        "secret",
		Tags:        nil,
		Restrictions: secretGroupRestrictionsView{
			Restricted:        true,
			Tags:              []string{"backend"},
			TagMatchCondition: &or,
		},
		Variables: map[string]string{"A": "1"},
		CreatedAt: time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC),
	})

	if diags.HasError() {
		t.Fatal(diags)
	}

	if !model.Tags.Equal(types.SetValueMust(types.StringType, nil)) {
		t.Errorf("tags = %v, want an empty set, matching the schema default", model.Tags)
	}

	if model.UpdatedAt != types.StringNull() || model.CreatedAt != types.StringValue("2024-05-01T00:00:00Z") {
		t.Errorf("timestamps = %v / %v", model.CreatedAt, model.UpdatedAt)
	}

	var r secretGroupRestrictionsModel

	if d := model.Restrictions.As(ctx, &r, objectAsOptions); d.HasError() {
		t.Fatal(d)
	}

	// Recorded as reported, since there was no configuration to compare with
	// (as after an import).
	if r.TagMatchCondition != types.StringValue("or") {
		t.Errorf("tag_match_condition = %v, want or", r.TagMatchCondition)
	}

	if !r.Resources.IsNull() || !r.StageIDs.IsNull() {
		t.Errorf("empty lists should be null: resources %v, stage_ids %v", r.Resources, r.StageIDs)
	}

	variables := map[string]string{}
	model.Variables.ElementsAs(ctx, &variables, false)

	if !reflect.DeepEqual(variables, map[string]string{"A": "1"}) {
		t.Errorf("variables = %v", variables)
	}
}

// Northflank may fill in a tag match condition the configuration left out;
// recording it would make Terraform report an inconsistent result.
func TestApplySecretGroupKeepsUnsetTagMatchCondition(t *testing.T) {
	ctx := t.Context()
	or := "or"

	model := secretGroupResourceModel{
		Restrictions: restrictionsObject(t, true, nil, []string{"backend"}, nil),
	}

	applySecretGroup(ctx, &model, &secretGroupView{
		Restrictions: secretGroupRestrictionsView{Restricted: true, Tags: []string{"backend"}, TagMatchCondition: &or},
	})

	var r secretGroupRestrictionsModel
	model.Restrictions.As(ctx, &r, objectAsOptions)

	if !r.TagMatchCondition.IsNull() {
		t.Errorf("tag_match_condition = %v, want it left unset", r.TagMatchCondition)
	}
}

func TestApplySecretGroupUnrestrictedIsNull(t *testing.T) {
	model := secretGroupResourceModel{}
	applySecretGroup(t.Context(), &model, &secretGroupView{Variables: map[string]string{}})

	if !model.Restrictions.IsNull() {
		t.Errorf("restrictions = %v, want null for an unrestricted group", model.Restrictions)
	}

	if !model.Variables.IsNull() {
		t.Errorf("variables = %v, want null when there are none", model.Variables)
	}
}

func TestKeptVariables(t *testing.T) {
	got := keptVariables(map[string]string{"A": "1"}, map[string]string{"A": "1", "C": "3", "B": "2"})

	if want := []string{"B", "C"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keptVariables() = %v, want %v", got, want)
	}

	if got := keptVariables(map[string]string{"A": "1"}, map[string]string{"A": "2"}); got != nil {
		t.Errorf("keptVariables() = %v, want none for a changed value", got)
	}
}

func TestSecretGroupImportState(t *testing.T) {
	ctx := t.Context()
	r := NewSecretGroupResource().(*secretGroupResource)

	schemaResp := &fwresource.SchemaResponse{}
	r.Schema(ctx, fwresource.SchemaRequest{}, schemaResp)

	newState := func() tfsdk.State {
		return tfsdk.State{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
		}
	}

	t.Run("valid", func(t *testing.T) {
		resp := &fwresource.ImportStateResponse{State: newState()}
		r.ImportState(ctx, fwresource.ImportStateRequest{ID: "my-project/app-secrets"}, resp)

		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}

		var projectID, id types.String
		resp.State.GetAttribute(ctx, pathRoot("project_id"), &projectID)
		resp.State.GetAttribute(ctx, pathRoot("id"), &id)

		if projectID.ValueString() != "my-project" || id.ValueString() != "app-secrets" {
			t.Errorf("imported project_id %v, id %v", projectID, id)
		}
	})

	for _, bad := range []string{"app-secrets", "/app-secrets", "my-project/", "a/b/c"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			resp := &fwresource.ImportStateResponse{State: newState()}
			r.ImportState(ctx, fwresource.ImportStateRequest{ID: bad}, resp)

			if !resp.Diagnostics.HasError() {
				t.Errorf("ImportState(%q) succeeded, want an error", bad)
			}
		})
	}
}

func TestSecretGroupPatterns(t *testing.T) {
	tests := []struct {
		name  string
		match func(string) bool
		value string
		want  bool
	}{
		{"name with spaces", secretGroupNamePattern.MatchString, "App Secrets 2", true},
		{"name with trailing space", secretGroupNamePattern.MatchString, "App ", false},
		{"identifier", secretGroupIdentifierPattern.MatchString, "prod-eu", true},
		{"identifier with double hyphen", secretGroupIdentifierPattern.MatchString, "prod--eu", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.match(tt.value); got != tt.want {
				t.Errorf("match(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
