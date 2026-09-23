package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Regression: a job configured with account_login = "gofieldnode" failed
// with "Provider produced inconsistent result after apply" because the API
// left accountLogin out and the read-back recorded null.
func TestRepositoryKeepsConfiguredAccountTheAPIOmits(t *testing.T) {
	ctx := t.Context()
	login := "gofieldnode"
	configured := repositoryObject(&login)

	service := &serviceRead{}
	service.VcsData = &struct {
		ProjectURL      string  `json:"projectUrl"`
		ProjectType     string  `json:"projectType"`
		ProjectBranch   *string `json:"projectBranch"`
		AccountLogin    *string `json:"accountLogin"`
		VcsLinkID       *string `json:"vcsLinkId"`
		SelfHostedVcsID *string `json:"selfHostedVcsId"`
	}{ProjectURL: "https://github.com/acme/api", ProjectType: "github"}

	value, diags := repositoryValue(ctx, configured, service)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if !value.Equal(configured) {
		t.Errorf("repository = %v, want the configured account kept", value)
	}
}

func TestReportedOptional(t *testing.T) {
	picked, other, empty := "picked", "other", ""
	set, unset := types.StringValue("mine"), types.StringNull()

	for name, tt := range map[string]struct {
		prior    types.String
		reported *string
		hasPrior bool
		want     types.String
	}{
		"import records what is reported":   {unset, &picked, false, types.StringValue("picked")},
		"import with nothing reported":      {unset, nil, false, types.StringNull()},
		"unset stays unset though reported": {unset, &picked, true, types.StringNull()},
		"set kept when not reported":        {set, nil, true, set},
		"set kept when reported empty":      {set, &empty, true, set},
		"set and reported the same":         {set, ptr("mine"), true, set},
		"set but reported otherwise: drift": {set, &other, true, types.StringValue("other")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := reportedOptional(tt.prior, tt.reported, tt.hasPrior); !got.Equal(tt.want) {
				t.Errorf("reportedOptional() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Regression: a secret group created with type = "secret" failed with
// "Provider produced inconsistent result after apply" because the API left
// type out of its response, which read as "".
func TestSecretGroupKeepsTypeTheAPIOmits(t *testing.T) {
	ctx := t.Context()

	model := secretGroupResourceModel{
		Type:         types.StringValue("secret"),
		SecretType:   types.StringValue("environment"),
		Restrictions: types.ObjectNull(secretGroupRestrictionsAttrTypes),
	}

	// Both left out of the response.
	applySecretGroup(ctx, &model, &secretGroupView{ID: "shared", Name: "Shared"})

	if model.Type.ValueString() != "secret" || model.SecretType.ValueString() != "environment" {
		t.Errorf("type %v, secret_type %v; want the configured values kept", model.Type, model.SecretType)
	}

	// Reported: taken, so a change made outside Terraform shows.
	applySecretGroup(ctx, &model, &secretGroupView{ID: "shared", Name: "Shared", Type: "config", SecretType: "arguments"})

	if model.Type.ValueString() != "config" || model.SecretType.ValueString() != "arguments" {
		t.Errorf("type %v, secret_type %v; want the reported values", model.Type, model.SecretType)
	}

	// After an import there is nothing to keep: fall back to the defaults.
	imported := secretGroupResourceModel{Restrictions: types.ObjectNull(secretGroupRestrictionsAttrTypes)}
	applySecretGroup(ctx, &imported, &secretGroupView{ID: "shared", Name: "Shared"})

	if imported.Type.ValueString() != secretGroupDefaultType || imported.SecretType.ValueString() != secretGroupDefaultSecretType {
		t.Errorf("imported type %v, secret_type %v; want the defaults", imported.Type, imported.SecretType)
	}
}

func ptr(s string) *string { return &s }
