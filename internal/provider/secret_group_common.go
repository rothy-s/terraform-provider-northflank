package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Enumerations and constraints copied from the OpenAPI document.
var (
	secretGroupSecretTypes       = []string{"environment-arguments", "environment", "arguments"}
	secretGroupTypes             = []string{"secret", "config"}
	secretGroupResourceTypes     = []string{"service", "job"}
	secretGroupTagMatchCondition = []string{"and", "or"}

	secretGroupNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]+((-|\s)[a-zA-Z0-9]+)*$`)
	// Tags, environment IDs and secret group IDs share one identifier shape.
	secretGroupIdentifierPattern = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)
	secretGroupObjectIDPattern   = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

// Defaults applied when the configuration leaves them out. Priority is
// required by the API; 10 is what the Northflank dashboard suggests.
const (
	secretGroupDefaultPriority   = 10
	secretGroupDefaultSecretType = "environment-arguments"
	secretGroupDefaultType       = "secret"
)

// secretGroupPayload is the JSON body shared by create and update. The two
// generated body types describe the same JSON with different nested types,
// which makes building either of them by hand painful; this struct marshals
// to what both accept, and a test checks it against the generated types.
type secretGroupPayload struct {
	Name         string                          `json:"name,omitempty"`
	Description  *string                         `json:"description,omitempty"`
	Priority     *int                            `json:"priority,omitempty"`
	SecretType   *string                         `json:"secretType,omitempty"`
	Type         *string                         `json:"type,omitempty"`
	Tags         *[]string                       `json:"tags,omitempty"`
	Restrictions *secretGroupRestrictionsPayload `json:"restrictions,omitempty"`
	Secrets      *secretGroupSecretsPayload      `json:"secrets,omitempty"`
}

type secretGroupRestrictionsPayload struct {
	Restricted        bool                       `json:"restricted"`
	NfObjects         []secretGroupObjectPayload `json:"nfObjects"`
	Tags              []string                   `json:"tags"`
	TagMatchCondition *string                    `json:"tagMatchCondition,omitempty"`
	StageIds          []string                   `json:"stageIds"`
}

type secretGroupObjectPayload struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type secretGroupSecretsPayload struct {
	Variables map[string]string `json:"variables"`

	// Left out unless configured, so that mounts made in the dashboard
	// survive updates of groups that do not manage them.
	DockerSecretMounts *map[string]secretGroupContentPayload `json:"dockerSecretMounts,omitempty"`
}

// secretGroupContentPayload is the content of a Docker secret mount. The API
// only takes base64 data; the encoding describes the decoded content.
type secretGroupContentPayload struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding"`
}

// dockerSecretMountsPayload encodes the configured mounts, or returns nil when
// none are configured.
func dockerSecretMountsPayload(ctx context.Context, value types.Map, diags *diag.Diagnostics) *map[string]secretGroupContentPayload {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}

	var contents map[string]string
	diags.Append(value.ElementsAs(ctx, &contents, false)...)

	mounts := make(map[string]secretGroupContentPayload, len(contents))

	for id, content := range contents {
		mounts[id] = secretGroupContentPayload{
			Data:     base64.StdEncoding.EncodeToString([]byte(content)),
			Encoding: "utf-8",
		}
	}

	return &mounts
}

// secretGroupView is the part of a secret group the provider exposes, in one
// named shape for the resource, the data source and the tests.
type secretGroupView struct {
	ID           string
	ProjectID    string
	Name         string
	Description  *string
	Priority     int
	SecretType   string
	Type         string
	Tags         []string
	Restrictions secretGroupRestrictionsView
	Variables    map[string]string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type secretGroupRestrictionsView struct {
	Restricted        bool
	Objects           []secretGroupObjectPayload
	Tags              []string
	TagMatchCondition *string
	StageIDs          []string
}

// isEmpty reports whether the restrictions amount to "not restricted", which
// is how the API reports a group that was created without any.
func (r secretGroupRestrictionsView) isEmpty() bool {
	return !r.Restricted && len(r.Objects) == 0 && len(r.Tags) == 0 && len(r.StageIDs) == 0
}

// getSecretGroup fetches a secret group with only its own secrets, not those
// inherited from linked addons, retrying transient failures.
func getSecretGroup(ctx context.Context, client *northflank.ClientWithResponses, projectID, secretID string) (*secretGroupView, error) {
	show := northflank.GetProjectsSecretsSecretidParamsShowThis
	params := &northflank.GetProjectsSecretsSecretidParams{Show: &show}

	var view *secretGroupView

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsSecretsSecretidWithResponse(ctx, projectID, secretID, params)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		view, err = secretGroupFromGet(got)
		return err
	})

	return view, err
}

func secretGroupFromGet(got *northflank.GetProjectsSecretsSecretidHTTPResponse) (*secretGroupView, error) {
	if got.JSON200 == nil {
		return nil, errors.New("the API returned a successful status with no secret group body")
	}

	data := got.JSON200.Data
	r := data.Restrictions

	view := &secretGroupView{
		ID:          data.Id,
		ProjectID:   data.ProjectId,
		Name:        data.Name,
		Description: data.Description,
		Priority:    data.Priority,
		SecretType:  string(data.SecretType),
		Type:        string(data.Type),
		Tags:        data.Tags,
		Restrictions: secretGroupRestrictionsView{
			Restricted:        r.Restricted != nil && *r.Restricted,
			Tags:              derefStrings(r.Tags),
			StageIDs:          derefStrings(r.StageIds),
			TagMatchCondition: (*string)(r.TagMatchCondition),
		},
		CreatedAt: data.CreatedAt,
		UpdatedAt: data.UpdatedAt,
	}

	if r.NfObjects != nil {
		for _, o := range *r.NfObjects {
			view.Restrictions.Objects = append(view.Restrictions.Objects,
				secretGroupObjectPayload{ID: o.Id, Type: string(o.Type)})
		}
	}

	variables, err := secretVariables(data.Secrets)

	if err != nil {
		return nil, err
	}

	view.Variables = variables
	return view, nil
}

// secretVariables pulls the variables out of the loosely typed secrets object.
// The spec does not describe its contents, so they are decoded by hand.
func secretVariables(secrets map[string]any) (map[string]string, error) {
	raw, ok := secrets["variables"]

	if !ok || raw == nil {
		return map[string]string{}, nil
	}

	values, ok := raw.(map[string]any)

	if !ok {
		return nil, fmt.Errorf("unexpected secret variables of type %T", raw)
	}

	out := make(map[string]string, len(values))

	for k, v := range values {
		s, ok := v.(string)

		if !ok {
			return nil, fmt.Errorf("secret variable %q has a value of type %T, want a string", k, v)
		}

		out[k] = s
	}

	return out, nil
}

// objectAsOptions converts objects strictly: every attribute must be present.
var objectAsOptions = basetypes.ObjectAsOptions{}

// secretGroupObjectAttrTypes describes one element of restrictions.resources.
var secretGroupObjectAttrTypes = map[string]attr.Type{
	"id":   types.StringType,
	"type": types.StringType,
}

// secretGroupRestrictionsAttrTypes describes the restrictions object.
var secretGroupRestrictionsAttrTypes = map[string]attr.Type{
	"restricted":          types.BoolType,
	"resources":           types.SetType{ElemType: types.ObjectType{AttrTypes: secretGroupObjectAttrTypes}},
	"tags":                types.SetType{ElemType: types.StringType},
	"tag_match_condition": types.StringType,
	"stage_ids":           types.SetType{ElemType: types.StringType},
}

// secretGroupRestrictionsModel maps the restrictions object.
type secretGroupRestrictionsModel struct {
	Restricted        types.Bool   `tfsdk:"restricted"`
	Resources         types.Set    `tfsdk:"resources"`
	Tags              types.Set    `tfsdk:"tags"`
	TagMatchCondition types.String `tfsdk:"tag_match_condition"`
	StageIDs          types.Set    `tfsdk:"stage_ids"`
}

type secretGroupObjectModel struct {
	ID   types.String `tfsdk:"id"`
	Type types.String `tfsdk:"type"`
}

// restrictionsValue converts the API's restrictions into Terraform's shape.
// "Not restricted at all" becomes null, and so does each empty list, which is
// how the API reports what the configuration left out.
func restrictionsValue(ctx context.Context, r secretGroupRestrictionsView) (types.Object, diag.Diagnostics) {
	if r.isEmpty() {
		return types.ObjectNull(secretGroupRestrictionsAttrTypes), nil
	}

	var diags diag.Diagnostics

	resources := types.SetNull(types.ObjectType{AttrTypes: secretGroupObjectAttrTypes})

	if len(r.Objects) > 0 {
		objects := make([]secretGroupObjectModel, 0, len(r.Objects))

		for _, o := range r.Objects {
			objects = append(objects, secretGroupObjectModel{ID: types.StringValue(o.ID), Type: types.StringValue(o.Type)})
		}

		var d diag.Diagnostics
		resources, d = types.SetValueFrom(ctx, types.ObjectType{AttrTypes: secretGroupObjectAttrTypes}, objects)
		diags.Append(d...)
	}

	model := secretGroupRestrictionsModel{
		Restricted:        types.BoolValue(r.Restricted),
		Resources:         resources,
		Tags:              stringSetOrNull(ctx, r.Tags, &diags),
		TagMatchCondition: types.StringPointerValue(r.TagMatchCondition),
		StageIDs:          stringSetOrNull(ctx, r.StageIDs, &diags),
	}

	value, d := types.ObjectValueFrom(ctx, secretGroupRestrictionsAttrTypes, model)
	diags.Append(d...)

	return value, diags
}

// restrictionsPayload converts configured restrictions into the API's shape.
// A null value means "not restricted", which is sent explicitly so that
// removing the block from configuration lifts existing restrictions.
func restrictionsPayload(ctx context.Context, value types.Object) (*secretGroupRestrictionsPayload, diag.Diagnostics) {
	payload := &secretGroupRestrictionsPayload{
		NfObjects: []secretGroupObjectPayload{},
		Tags:      []string{},
		StageIds:  []string{},
	}

	if value.IsNull() || value.IsUnknown() {
		return payload, nil
	}

	var diags diag.Diagnostics
	var model secretGroupRestrictionsModel

	diags.Append(value.As(ctx, &model, objectAsOptions)...)

	if diags.HasError() {
		return nil, diags
	}

	payload.Restricted = model.Restricted.ValueBool()
	payload.TagMatchCondition = knownStringPointer(model.TagMatchCondition)

	if !model.Resources.IsNull() && !model.Resources.IsUnknown() {
		var objects []secretGroupObjectModel
		diags.Append(model.Resources.ElementsAs(ctx, &objects, false)...)

		for _, o := range objects {
			payload.NfObjects = append(payload.NfObjects,
				secretGroupObjectPayload{ID: o.ID.ValueString(), Type: o.Type.ValueString()})
		}

		sort.Slice(payload.NfObjects, func(i, j int) bool {
			return payload.NfObjects[i].Type+"/"+payload.NfObjects[i].ID <
				payload.NfObjects[j].Type+"/"+payload.NfObjects[j].ID
		})
	}

	payload.Tags = stringsFromSet(ctx, model.Tags, &diags)
	payload.StageIds = stringsFromSet(ctx, model.StageIDs, &diags)

	return payload, diags
}

func stringSetOrNull(ctx context.Context, values []string, diags *diag.Diagnostics) types.Set {
	if len(values) == 0 {
		return types.SetNull(types.StringType)
	}

	set, d := types.SetValueFrom(ctx, types.StringType, values)
	diags.Append(d...)

	return set
}

// stringsFromSet returns the sorted elements of a known set, or an empty
// slice for null or unknown.
func stringsFromSet(ctx context.Context, set types.Set, diags *diag.Diagnostics) []string {
	out := []string{}

	if set.IsNull() || set.IsUnknown() {
		return out
	}

	diags.Append(set.ElementsAs(ctx, &out, false)...)
	sort.Strings(out)

	return out
}

func derefStrings(p *[]string) []string {
	if p == nil {
		return nil
	}

	return *p
}
