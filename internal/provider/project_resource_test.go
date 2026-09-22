package provider

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The fallback state written when the post-create read-back fails must not
// contain unknown values, or Terraform rejects it and the project is leaked.
func TestApplyCreateResponseLeavesNothingUnknown(t *testing.T) {
	model := projectResourceModel{
		Name: types.StringValue("My Project"),
		// Computed attributes arrive from the plan as unknown.
		ID:                 types.StringUnknown(),
		UID:                types.StringUnknown(),
		Description:        types.StringUnknown(),
		Color:              types.StringUnknown(),
		Region:             types.StringUnknown(),
		ClusterID:          types.StringUnknown(),
		CreatedAt:          types.StringUnknown(),
		DeleteChildObjects: types.BoolValue(false),
	}

	color, region := "#EF233C", "europe-west"
	applyCreateResponse(&model, "my-project", nil, &color, &region, nil)

	unknowns := map[string]bool{
		"id":          model.ID.IsUnknown(),
		"uid":         model.UID.IsUnknown(),
		"name":        model.Name.IsUnknown(),
		"description": model.Description.IsUnknown(),
		"color":       model.Color.IsUnknown(),
		"region":      model.Region.IsUnknown(),
		"cluster_id":  model.ClusterID.IsUnknown(),
		"created_at":  model.CreatedAt.IsUnknown(),
	}

	for attr, isUnknown := range unknowns {
		if isUnknown {
			t.Errorf("%s is still unknown after applyCreateResponse", attr)
		}
	}

	if model.ID != types.StringValue("my-project") {
		t.Errorf("id = %v, want my-project", model.ID)
	}

	if model.Color != types.StringValue(color) {
		t.Errorf("color = %v, want the color Northflank assigned", model.Color)
	}
}

func TestApplyProject(t *testing.T) {
	description := "The project description"
	created := time.Date(2021, 1, 20, 11, 19, 53, 0, time.UTC)

	model := projectResourceModel{
		Color:              types.StringValue("#EF233C"),
		DeleteChildObjects: types.BoolValue(true),
	}

	applyProject(&model, &projectView{
		ID:          "my-project",
		UID:         "68f0aa1b9c4e2d7f11a3b402",
		Name:        "My Project",
		Description: &description,
		Region:      "europe-west",
		ClusterID:   "nf-europe-west",
		CreatedAt:   created,
	})

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"id", model.ID, types.StringValue("my-project")},
		{"uid", model.UID, types.StringValue("68f0aa1b9c4e2d7f11a3b402")},
		{"name", model.Name, types.StringValue("My Project")},
		{"description", model.Description, types.StringValue(description)},
		{"region", model.Region, types.StringValue("europe-west")},
		{"cluster_id", model.ClusterID, types.StringValue("nf-europe-west")},
		{"created_at", model.CreatedAt, types.StringValue("2021-01-20T11:19:53Z")},
		// Not reported by the API, so kept from the model.
		{"color", model.Color, types.StringValue("#EF233C")},
		{"delete_child_objects", model.DeleteChildObjects, types.BoolValue(true)},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// After import the model holds only an id; nothing may stay unknown or null
// where the schema promises a value.
func TestApplyProjectAfterImport(t *testing.T) {
	model := projectResourceModel{
		ID:                 types.StringValue("my-project"),
		Color:              types.StringUnknown(),
		DeleteChildObjects: types.BoolNull(),
	}

	applyProject(&model, &projectView{ID: "my-project", Name: "My Project"})

	if !model.Color.IsNull() {
		t.Errorf("color = %v, want null", model.Color)
	}

	if model.Description != types.StringNull() {
		t.Errorf("description = %v, want null for a project without one", model.Description)
	}

	if model.DeleteChildObjects != types.BoolValue(false) {
		t.Errorf("delete_child_objects = %v, want false", model.DeleteChildObjects)
	}
}

func TestProjectCreateBody(t *testing.T) {
	tests := []struct {
		name  string
		model projectResourceModel
		want  map[string]any
	}{
		{
			name: "region project",
			model: projectResourceModel{
				Name:        types.StringValue("My Project"),
				Description: types.StringValue("hello"),
				Color:       types.StringUnknown(),
				Region:      types.StringValue("europe-west"),
				ClusterID:   types.StringUnknown(),
			},
			want: map[string]any{"name": "My Project", "description": "hello", "region": "europe-west"},
		},
		{
			name: "BYOC project",
			model: projectResourceModel{
				Name:        types.StringValue("My Project"),
				Description: types.StringUnknown(),
				Color:       types.StringValue("#fff"),
				Region:      types.StringUnknown(),
				ClusterID:   types.StringValue("gcp-cluster-1"),
			},
			want: map[string]any{"name": "My Project", "color": "#fff", "clusterId": "gcp-cluster-1"},
		},
		{
			name: "defaults left to Northflank",
			model: projectResourceModel{
				Name:        types.StringValue("My Project"),
				Description: types.StringUnknown(),
				Color:       types.StringUnknown(),
				Region:      types.StringUnknown(),
				ClusterID:   types.StringUnknown(),
			},
			want: map[string]any{"name": "My Project"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := projectCreateBody(&tt.model)

			if err != nil {
				t.Fatalf("projectCreateBody() error = %v", err)
			}

			raw, err := json.Marshal(body)

			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var got map[string]any

			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}

			if len(got) != len(tt.want) {
				t.Errorf("body = %s, want exactly %v", raw, tt.want)
			}

			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("body[%q] = %v, want %v (body %s)", k, got[k], v, raw)
				}
			}
		})
	}
}

func TestProjectPatchBody(t *testing.T) {
	state := projectResourceModel{
		Description: types.StringValue("old"),
		Color:       types.StringValue("#000"),
	}

	t.Run("no change", func(t *testing.T) {
		plan := state

		if _, changed := projectPatchBody(&plan, &state); changed {
			t.Error("changed = true for an identical plan")
		}
	})

	t.Run("removed from configuration keeps the value", func(t *testing.T) {
		plan := projectResourceModel{Description: types.StringUnknown(), Color: types.StringUnknown()}

		if _, changed := projectPatchBody(&plan, &state); changed {
			t.Error("changed = true for unknown values")
		}
	})

	t.Run("only changed fields are sent", func(t *testing.T) {
		plan := state
		plan.Color = types.StringValue("#fff")

		body, changed := projectPatchBody(&plan, &state)

		if !changed {
			t.Fatal("changed = false, want true")
		}

		if body.Description != nil {
			t.Errorf("description = %q, want it omitted", *body.Description)
		}

		if body.Color == nil || *body.Color != "#fff" {
			t.Errorf("color = %v, want #fff", body.Color)
		}
	})
}

// The patterns are copied from JavaScript regular expressions in the spec, so
// check they still mean the same thing under RE2.
func TestProjectPatterns(t *testing.T) {
	tests := []struct {
		name  string
		match func(string) bool
		value string
		want  bool
	}{
		{"name with space", projectNamePattern.MatchString, "New Project", true},
		{"name with hyphen", projectNamePattern.MatchString, "new-project-2", true},
		{"name starting with digit", projectNamePattern.MatchString, "2cool", false},
		{"name with double space", projectNamePattern.MatchString, "a  b", false},
		{"description with punctuation", projectDescriptionPattern.MatchString, `It's [a] (test): 100% done!`, true},
		{"description with emoji", projectDescriptionPattern.MatchString, "rocket 🚀", false},
		{"long color", projectColorPattern.MatchString, "#EF233C", true},
		{"short color", projectColorPattern.MatchString, "#e23", true},
		{"color without hash", projectColorPattern.MatchString, "EF233C", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.match(tt.value); got != tt.want {
				t.Errorf("match(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
