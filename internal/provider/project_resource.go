package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ resource.Resource                = (*projectResource)(nil)
	_ resource.ResourceWithConfigure   = (*projectResource)(nil)
	_ resource.ResourceWithImportState = (*projectResource)(nil)
)

type projectResource struct {
	client *northflank.ClientWithResponses
}

// NewProjectResource returns the northflank_project resource implementation.
func NewProjectResource() resource.Resource {
	return &projectResource{}
}

// projectResourceModel maps the northflank_project schema.
type projectResourceModel struct {
	ID          types.String `tfsdk:"id"`
	UID         types.String `tfsdk:"uid"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Color       types.String `tfsdk:"color"`
	Region      types.String `tfsdk:"region"`
	ClusterID   types.String `tfsdk:"cluster_id"`
	CreatedAt   types.String `tfsdk:"created_at"`

	DeleteChildObjects types.Bool `tfsdk:"delete_child_objects"`
}

func (r *projectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

func (r *projectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Northflank project, the container for services, jobs, addons and secrets. " +
			"A project runs either in a Northflank-managed `region` or on a bring-your-own-cloud cluster " +
			"(`cluster_id`).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Identifier of the project, derived by Northflank from `name` (for example " +
					"`my-project`). Used to address the project in the API and in other resources.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"uid": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Permanent identifier of the project. Unlike `id`, it never changes and is " +
					"never reused.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the project, 3 to 39 characters. Changing this forces a new " +
					"project, since Northflank derives the project `id` from it and cannot rename a project.",
				Validators: projectNameValidators(),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Short description of the project, up to 200 characters. Removing it from " +
					"configuration keeps the current value, since the API cannot clear it.",
				Validators: projectDescriptionValidators(),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"color": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Hex color of the project in the Northflank dashboard, such as `#EF233C`. " +
					"Northflank picks one when this is not set. The API does not report the color when reading " +
					"a project, so a change made outside Terraform is not detected, and an imported project " +
					"shows it as unset until configured.",
				Validators: projectColorValidators(),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Northflank region to host the project in, such as `europe-west`. Northflank " +
					"picks a default when neither this nor `cluster_id` is set. Conflicts with `cluster_id`. " +
					"Changing this forces a new project.",
				Validators: []validator.String{
					stringvalidator.ConflictsWith(path.MatchRoot("cluster_id")),
				},
				// UseStateForUnknown must run first: an attribute removed from
				// configuration plans as unknown, which RequiresReplace would
				// otherwise treat as a change.
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cluster_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Bring-your-own-cloud cluster to host the project on. Conflicts with " +
					"`region`. When the project runs in a Northflank region, this reports the managed cluster " +
					"serving it. Changing this forces a new project.",
				Validators: []validator.String{
					stringvalidator.ConflictsWith(path.MatchRoot("region")),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation time, in RFC 3339 format.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"delete_child_objects": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether destroying this resource also deletes the services, jobs, addons " +
					"and other resources inside the project. When `false`, the default, destroying a project " +
					"that is not empty fails instead. Only affects destroy; it is never sent to Northflank " +
					"otherwise.",
			},
		},
	}
}

func (r *projectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *projectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan projectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, err := projectCreateBody(&plan)

	if err != nil {
		resp.Diagnostics.AddError("Error building Northflank project request", err.Error())
		return
	}

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostProjectsWithResponse(ctx, body)

	if err != nil {
		resp.Diagnostics.AddError("Error creating Northflank project", err.Error())
		return
	}

	if err := northflank.CheckResponse(created); err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank project",
			fmt.Sprintf("Could not create project %q: %s", plan.Name.ValueString(), err),
		)
		return
	}

	if created.JSON200 == nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank project",
			"The API returned a successful status with no project body.",
		)
		return
	}

	data := created.JSON200.Data
	tflog.Debug(ctx, "created Northflank project", map[string]any{"id": data.Id})

	// The project exists from here on, so every remaining failure path still
	// has to leave state behind -- otherwise Terraform forgets the project and
	// leaks it. Seeding the model from the create response leaves no computed
	// attribute unknown, which is what makes that state writable.
	applyCreateResponse(&plan, data.Id, data.Description, data.Color, data.Region, data.ClusterId)

	// Read back for the attributes the create response omits: uid, and the
	// region or cluster Northflank picked when none was given.
	project, err := getProject(ctx, r.client, data.Id)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back created Northflank project",
			fmt.Sprintf("The project was created with ID %q but could not be read: %s. "+
				"It has been saved to state; the remaining attributes are populated on the next refresh.",
				data.Id, err),
		)
		return
	}

	applyProject(&plan, project)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *projectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state projectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	project, err := getProject(ctx, r.client, state.ID.ValueString())

	if err != nil {
		if isNotFound(err) {
			tflog.Debug(ctx, "Northflank project no longer exists, removing from state",
				map[string]any{"id": state.ID.ValueString()})
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Error reading Northflank project",
			fmt.Sprintf("Could not read project %q: %s", state.ID.ValueString(), err),
		)
		return
	}

	applyProject(&state, project)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *projectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state projectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := state.ID.ValueString()
	body, changed := projectPatchBody(&plan, &state)

	if changed {
		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			updated, err := r.client.PatchProjectsProjectidWithResponse(ctx, projectID, body)

			if err != nil {
				return err
			}

			return northflank.CheckResponse(updated)
		})

		if err != nil {
			resp.Diagnostics.AddError(
				"Error updating Northflank project",
				fmt.Sprintf("Could not update project %q: %s", projectID, err),
			)
			return
		}
	}

	project, err := getProject(ctx, r.client, projectID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading Northflank project",
			fmt.Sprintf("Project %q was updated but could not be read back: %s", projectID, err),
		)
		return
	}

	applyProject(&plan, project)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *projectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state projectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := state.ID.ValueString()
	params := &northflank.DeleteProjectsProjectidParams{
		DeleteChildObjects: state.DeleteChildObjects.ValueBoolPointer(),
	}

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteProjectsProjectidWithResponse(ctx, projectID, params)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(deleted)
	})

	if err == nil || isNotFound(err) {
		// Gone either way, which is the desired end state.
		return
	}

	detail := fmt.Sprintf("Could not delete project %q: %s", projectID, err)

	if !state.DeleteChildObjects.ValueBool() {
		detail += "\n\nNorthflank refuses to delete a project that still contains resources. Remove them " +
			"first, or set delete_child_objects = true and apply before destroying to delete them along " +
			"with the project."
	}

	resp.Diagnostics.AddError("Error deleting Northflank project", detail)
}

func (r *projectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Read fills in everything else. delete_child_objects is not stored by
	// Northflank, so seed its default to avoid a spurious diff after import.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("delete_child_objects"), false)...)
}

// projectCreateBody builds the create request. The API takes one of two
// shapes: a project in a Northflank region, or one on a BYOC cluster.
func projectCreateBody(plan *projectResourceModel) (northflank.PostProjectsJSONRequestBody, error) {
	var body northflank.PostProjectsJSONRequestBody

	name := plan.Name.ValueString()
	description := knownStringPointer(plan.Description)
	color := knownStringPointer(plan.Color)

	if clusterID := knownStringPointer(plan.ClusterID); clusterID != nil {
		err := body.FromPostProjectsRequest1(northflank.PostProjectsRequest1{
			Name:        name,
			Description: description,
			Color:       color,
			ClusterId:   clusterID,
		})

		return body, err
	}

	err := body.FromPostProjectsRequest0(northflank.PostProjectsRequest0{
		Name:        name,
		Description: description,
		Color:       color,
		Region:      knownStringPointer(plan.Region),
	})

	return body, err
}

// projectPatchBody builds the update request from the attributes that
// changed, and reports whether there is anything to send. Unknown values mean
// the attribute was removed from configuration, which keeps the current value.
func projectPatchBody(plan, state *projectResourceModel) (northflank.PatchProjectsProjectidJSONRequestBody, bool) {
	var body northflank.PatchProjectsProjectidJSONRequestBody

	if v := knownStringPointer(plan.Description); v != nil && !plan.Description.Equal(state.Description) {
		body.Description = v
	}

	if v := knownStringPointer(plan.Color); v != nil && !plan.Color.Equal(state.Color) {
		body.Color = v
	}

	return body, body.Description != nil || body.Color != nil
}

// applyCreateResponse seeds the model from a create response, so that a state
// written before the read-back has no unknown values left in it. Attributes
// the create response does not report are set to null and pick up their real
// values on the next refresh.
func applyCreateResponse(model *projectResourceModel, id string, description, color, region, clusterID *string) {
	model.ID = types.StringValue(id)
	model.UID = types.StringNull()
	model.CreatedAt = types.StringNull()
	model.Description = types.StringPointerValue(description)
	model.Color = types.StringPointerValue(color)
	model.Region = types.StringPointerValue(region)
	model.ClusterID = types.StringPointerValue(clusterID)
}

// applyProject copies an API response onto the Terraform model. Color is not
// part of the response, so it keeps whatever the model already holds.
func applyProject(model *projectResourceModel, project *projectView) {
	model.ID = types.StringValue(project.ID)
	model.UID = types.StringValue(project.UID)
	model.Name = types.StringValue(project.Name)
	model.Description = types.StringPointerValue(project.Description)
	model.Region = types.StringValue(project.Region)
	model.ClusterID = types.StringValue(project.ClusterID)
	model.CreatedAt = timeValue(project.CreatedAt)

	if model.Color.IsUnknown() {
		model.Color = types.StringNull()
	}

	if model.DeleteChildObjects.IsNull() || model.DeleteChildObjects.IsUnknown() {
		model.DeleteChildObjects = types.BoolValue(false)
	}
}

// knownStringPointer returns the value of a string that is set and known, and
// nil for null or unknown.
func knownStringPointer(v types.String) *string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}

	return v.ValueStringPointer()
}
