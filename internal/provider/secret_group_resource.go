package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ resource.Resource                = (*secretGroupResource)(nil)
	_ resource.ResourceWithConfigure   = (*secretGroupResource)(nil)
	_ resource.ResourceWithImportState = (*secretGroupResource)(nil)
)

type secretGroupResource struct {
	client *northflank.ClientWithResponses
}

// NewSecretGroupResource returns the northflank_secret_group resource
// implementation.
func NewSecretGroupResource() resource.Resource {
	return &secretGroupResource{}
}

// secretGroupResourceModel maps the northflank_secret_group schema.
type secretGroupResourceModel struct {
	ID           types.String `tfsdk:"id"`
	ProjectID    types.String `tfsdk:"project_id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	Priority     types.Int64  `tfsdk:"priority"`
	SecretType   types.String `tfsdk:"secret_type"`
	Type         types.String `tfsdk:"type"`
	Tags         types.Set    `tfsdk:"tags"`
	Restrictions types.Object `tfsdk:"restrictions"`
	Variables    types.Map    `tfsdk:"variables"`
	CreatedAt    types.String `tfsdk:"created_at"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
}

func (r *secretGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret_group"
}

func (r *secretGroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a secret group in a Northflank project. A secret group holds environment " +
			"variables and build arguments that services and jobs in the project inherit, optionally restricted " +
			"to specific resources, tags or environments.\n\n" +
			"Variable values are stored in Terraform state. Treat the state file as a secret.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identifier of the secret group, derived by Northflank from `name`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the secret group belongs to. Changing this forces a new secret group.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the secret group, 3 to 100 characters. Changing this forces a new " +
					"secret group, since Northflank derives the `id` from it and cannot rename one.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 100),
					stringvalidator.RegexMatches(secretGroupNamePattern,
						"must contain only letters and digits, with single spaces or hyphens between them"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Short description of the secret group, up to 200 characters. Removing it " +
					"from configuration keeps the current value, since the API cannot clear it.",
				Validators: projectDescriptionValidators(),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"priority": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(secretGroupDefaultPriority),
				MarkdownDescription: fmt.Sprintf("Priority, from 0 to 100, used to merge secret groups that "+
					"define the same key: the higher priority wins. Defaults to `%d`.", secretGroupDefaultPriority),
				Validators: []validator.Int64{
					int64validator.Between(0, 100),
				},
			},
			"secret_type": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(secretGroupDefaultSecretType),
				MarkdownDescription: "Where the variables are injected: `environment-arguments` for both runtime " +
					"environment variables and build arguments, `environment` for runtime only, or `arguments` " +
					"for build arguments only. Defaults to `" + secretGroupDefaultSecretType + "`.",
				Validators: []validator.String{
					stringvalidator.OneOf(secretGroupSecretTypes...),
				},
			},
			"type": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(secretGroupDefaultType),
				MarkdownDescription: "Permission type of the group: `secret`, whose values need secret-level " +
					"permissions to view, or `config`. Defaults to `" + secretGroupDefaultType + "`.",
				Validators: []validator.String{
					stringvalidator.OneOf(secretGroupTypes...),
				},
			},
			"tags": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, nil)),
				MarkdownDescription: "Previously defined Northflank tags to attach to the secret group.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(secretGroupIdentifierValidators()...),
				},
			},
			"variables": schema.MapAttribute{
				Optional:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "Secret variables, as a map of name to value. Values are encrypted at rest " +
					"by Northflank, but also stored in Terraform state.",
				Validators: []validator.Map{
					mapvalidator.SizeAtLeast(1),
				},
			},
			"restrictions": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Limits which resources inherit the secret group. Leave out to make it " +
					"available to every service and job in the project.",
				Attributes: map[string]schema.Attribute{
					"restricted": schema.BoolAttribute{
						Required: true,
						MarkdownDescription: "Whether the restrictions apply. When `true`, only the resources, " +
							"tagged resources and environments listed here inherit the group.",
					},
					"resources": schema.SetNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Services and jobs the secret group is restricted to.",
						Validators: []validator.Set{
							setvalidator.SizeAtLeast(1),
						},
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"id": schema.StringAttribute{
									Required:            true,
									MarkdownDescription: "ID of the service or job.",
									Validators: []validator.String{
										stringvalidator.RegexMatches(secretGroupObjectIDPattern,
											"must contain only letters, digits and hyphens"),
									},
								},
								"type": schema.StringAttribute{
									Required:            true,
									MarkdownDescription: "Kind of resource: `service` or `job`.",
									Validators: []validator.String{
										stringvalidator.OneOf(secretGroupResourceTypes...),
									},
								},
							},
						},
					},
					"tags": schema.SetAttribute{
						Optional:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Tags a resource must carry to inherit the secret group.",
						Validators: []validator.Set{
							setvalidator.SizeAtLeast(1),
							setvalidator.ValueStringsAre(secretGroupIdentifierValidators()...),
						},
					},
					"tag_match_condition": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "Whether a resource needs `and` all of `tags`, or `or` any one of " +
							"them.",
						Validators: []validator.String{
							stringvalidator.OneOf(secretGroupTagMatchCondition...),
						},
					},
					"stage_ids": schema.SetAttribute{
						Optional:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "IDs of the environments the secret group is restricted to.",
						Validators: []validator.Set{
							setvalidator.SizeAtLeast(1),
							setvalidator.ValueStringsAre(secretGroupIdentifierValidators()...),
						},
					},
				},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation time, in RFC 3339 format.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Time of the last update, in RFC 3339 format.",
			},
		},
	}
}

func secretGroupIdentifierValidators() []validator.String {
	return []validator.String{
		stringvalidator.LengthBetween(3, 100),
		stringvalidator.RegexMatches(secretGroupIdentifierPattern,
			"must contain only letters and digits, with single hyphens between them"),
	}
}

func (r *secretGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *secretGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan secretGroupResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	payload, diags := secretGroupPayloadFrom(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	payload.Name = plan.Name.ValueString()
	projectID := plan.ProjectID.ValueString()

	body, err := json.Marshal(payload)

	if err != nil {
		resp.Diagnostics.AddError("Error building Northflank secret group request", err.Error())
		return
	}

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostProjectsSecretsWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))

	if err != nil {
		resp.Diagnostics.AddError("Error creating Northflank secret group", err.Error())
		return
	}

	if err := northflank.CheckResponse(created); err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank secret group",
			fmt.Sprintf("Could not create secret group %q in project %q: %s", payload.Name, projectID, err),
		)
		return
	}

	if created.JSON200 == nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank secret group",
			"The API returned a successful status with no secret group body.",
		)
		return
	}

	secretID := created.JSON200.Data.Id
	tflog.Debug(ctx, "created Northflank secret group", map[string]any{"project_id": projectID, "id": secretID})

	// The secret group exists from here on, so a failed read-back must still
	// leave state behind or Terraform leaks it. The plan already holds every
	// configured value; only the computed ones need filling in.
	plan.ID = types.StringValue(secretID)
	plan.Description = types.StringPointerValue(created.JSON200.Data.Description)
	plan.CreatedAt = types.StringNull()
	plan.UpdatedAt = types.StringNull()

	group, err := getSecretGroup(ctx, r.client, projectID, secretID)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back created Northflank secret group",
			fmt.Sprintf("The secret group was created with ID %q but could not be read: %s. "+
				"It has been saved to state; the remaining attributes are populated on the next refresh.",
				secretID, err),
		)
		return
	}

	resp.Diagnostics.Append(applySecretGroup(ctx, &plan, group)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state secretGroupResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, secretID := state.ProjectID.ValueString(), state.ID.ValueString()
	group, err := getSecretGroup(ctx, r.client, projectID, secretID)

	if err != nil {
		if isNotFound(err) {
			tflog.Debug(ctx, "Northflank secret group no longer exists, removing from state",
				map[string]any{"project_id": projectID, "id": secretID})
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Error reading Northflank secret group",
			fmt.Sprintf("Could not read secret group %q in project %q: %s", secretID, projectID, err),
		)
		return
	}

	resp.Diagnostics.Append(applySecretGroup(ctx, &state, group)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *secretGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state secretGroupResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// The whole desired state is sent every time, which makes the update
	// idempotent and therefore safe to retry.
	payload, diags := secretGroupPayloadFrom(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, err := json.Marshal(payload)

	if err != nil {
		resp.Diagnostics.AddError("Error building Northflank secret group request", err.Error())
		return
	}

	projectID, secretID := state.ProjectID.ValueString(), state.ID.ValueString()

	err = apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		updated, err := r.client.PatchProjectsSecretsSecretidWithBodyWithResponse(
			ctx, projectID, secretID, "application/json", bytes.NewReader(body))

		if err != nil {
			return err
		}

		return northflank.CheckResponse(updated)
	})

	if err != nil {
		resp.Diagnostics.AddError(
			"Error updating Northflank secret group",
			fmt.Sprintf("Could not update secret group %q in project %q: %s", secretID, projectID, err),
		)
		return
	}

	group, err := getSecretGroup(ctx, r.client, projectID, secretID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading Northflank secret group",
			fmt.Sprintf("Secret group %q was updated but could not be read back: %s", secretID, err),
		)
		return
	}

	if kept := keptVariables(payload.Secrets.Variables, group.Variables); len(kept) > 0 {
		resp.Diagnostics.AddWarning(
			"Northflank kept removed secret variables",
			fmt.Sprintf("These variables were removed from configuration but still exist on secret group %q "+
				"after the update: %s. State records them as they are, so the next plan will show the "+
				"difference again. Remove them in the Northflank dashboard, or report this as a provider bug.",
				secretID, strings.Join(kept, ", ")),
		)
	}

	resp.Diagnostics.Append(applySecretGroup(ctx, &plan, group)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secretGroupResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, secretID := state.ProjectID.ValueString(), state.ID.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteProjectsSecretsSecretidWithResponse(ctx, projectID, secretID)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(deleted)
	})

	if err == nil || isNotFound(err) {
		// Gone either way, which is the desired end state.
		return
	}

	resp.Diagnostics.AddError(
		"Error deleting Northflank secret group",
		fmt.Sprintf("Could not delete secret group %q in project %q: %s", secretID, projectID, err),
	)
}

func (r *secretGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, secretID, ok := strings.Cut(req.ID, "/")

	if !ok || projectID == "" || secretID == "" || strings.Contains(secretID, "/") {
		resp.Diagnostics.AddError(
			"Invalid import identifier",
			fmt.Sprintf("Expected <project_id>/<secret_group_id>, got %q. Import with: "+
				"terraform import northflank_secret_group.example my-project/my-secret-group", req.ID),
		)
		return
	}

	// Read fills in everything else.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), secretID)...)
}

// secretGroupPayloadFrom builds the request body from the plan, without the
// name, which only create accepts.
func secretGroupPayloadFrom(ctx context.Context, plan *secretGroupResourceModel) (*secretGroupPayload, diag.Diagnostics) {
	var diags diag.Diagnostics

	priority := int(plan.Priority.ValueInt64())
	secretType := plan.SecretType.ValueString()
	groupType := plan.Type.ValueString()
	tags := stringsFromSet(ctx, plan.Tags, &diags)

	restrictions, d := restrictionsPayload(ctx, plan.Restrictions)
	diags.Append(d...)

	// Always send the complete map, so that removed variables are dropped
	// rather than left behind.
	variables := map[string]string{}

	if !plan.Variables.IsNull() && !plan.Variables.IsUnknown() {
		diags.Append(plan.Variables.ElementsAs(ctx, &variables, false)...)
	}

	return &secretGroupPayload{
		Description:  knownStringPointer(plan.Description),
		Priority:     &priority,
		SecretType:   &secretType,
		Type:         &groupType,
		Tags:         &tags,
		Restrictions: restrictions,
		Secrets:      &secretGroupSecretsPayload{Variables: variables},
	}, diags
}

// applySecretGroup copies an API response onto the Terraform model.
func applySecretGroup(ctx context.Context, model *secretGroupResourceModel, group *secretGroupView) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(group.ID)
	model.ProjectID = types.StringValue(group.ProjectID)
	model.Name = types.StringValue(group.Name)
	model.Description = types.StringPointerValue(group.Description)
	model.Priority = types.Int64Value(int64(group.Priority))
	// The API has been seen leaving these out despite the spec marking them
	// required; keep the configured value rather than record an empty one.
	model.SecretType = reportedOrKept(model.SecretType, group.SecretType, secretGroupDefaultSecretType)
	model.Type = reportedOrKept(model.Type, group.Type, secretGroupDefaultType)
	model.CreatedAt = timeValue(group.CreatedAt)
	model.UpdatedAt = timeValue(group.UpdatedAt)

	tags, d := types.SetValueFrom(ctx, types.StringType, nonNil(group.Tags))
	diags.Append(d...)
	model.Tags = tags

	// Northflank may fill in a tag match condition the configuration left out.
	// Keep it unset in that case, or Terraform reports an inconsistent result.
	if group.Restrictions.TagMatchCondition != nil && !tagMatchConditionConfigured(ctx, model.Restrictions) {
		group.Restrictions.TagMatchCondition = nil
	}

	restrictions, d := restrictionsValue(ctx, group.Restrictions)
	diags.Append(d...)
	model.Restrictions = restrictions

	// An empty map is how the API reports "no variables", which is what a
	// configuration without the attribute means.
	if len(group.Variables) == 0 {
		model.Variables = types.MapNull(types.StringType)
	} else {
		variables, d := types.MapValueFrom(ctx, types.StringType, group.Variables)
		diags.Append(d...)
		model.Variables = variables
	}

	return diags
}

// tagMatchConditionConfigured reports whether the model's restrictions set a
// tag match condition. A null model, as after import, counts as configured,
// so that import records whatever Northflank reports.
func tagMatchConditionConfigured(ctx context.Context, restrictions types.Object) bool {
	if restrictions.IsNull() || restrictions.IsUnknown() {
		return true
	}

	var model secretGroupRestrictionsModel

	if diags := restrictions.As(ctx, &model, objectAsOptions); diags.HasError() {
		return true
	}

	return !model.TagMatchCondition.IsNull()
}

// keptVariables lists the variables the API still reports that were not in
// the desired set, in sorted order.
func keptVariables(desired, actual map[string]string) []string {
	var kept []string

	for name := range actual {
		if _, ok := desired[name]; !ok {
			kept = append(kept, name)
		}
	}

	sort.Strings(kept)
	return kept
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}

	return values
}

// reportedOrKept takes a reported value, or, when the API leaves it out, the
// known prior value, or failing that the schema default.
func reportedOrKept(prior types.String, reported, fallback string) types.String {
	switch {
	case reported != "":
		return types.StringValue(reported)
	case !prior.IsNull() && !prior.IsUnknown():
		return prior
	default:
		return types.StringValue(fallback)
	}
}
