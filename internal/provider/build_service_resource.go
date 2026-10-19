package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ resource.Resource                = (*buildServiceResource)(nil)
	_ resource.ResourceWithConfigure   = (*buildServiceResource)(nil)
	_ resource.ResourceWithImportState = (*buildServiceResource)(nil)
)

type buildServiceResource struct {
	client *northflank.ClientWithResponses
}

// NewBuildServiceResource returns the northflank_build_service resource
// implementation.
func NewBuildServiceResource() resource.Resource {
	return &buildServiceResource{}
}

// buildServiceResourceModel maps the northflank_build_service schema.
type buildServiceResourceModel struct {
	ID                 types.String `tfsdk:"id"`
	UID                types.String `tfsdk:"uid"`
	AppID              types.String `tfsdk:"app_id"`
	ProjectID          types.String `tfsdk:"project_id"`
	StageID            types.String `tfsdk:"stage_id"`
	Architecture       types.String `tfsdk:"architecture"`
	Name               types.String `tfsdk:"name"`
	Description        types.String `tfsdk:"description"`
	Tags               types.Set    `tfsdk:"tags"`
	BuildPlan          types.String `tfsdk:"build_plan"`
	Repository         types.Object `tfsdk:"repository"`
	Build              types.Object `tfsdk:"build"`
	BranchRestrictions types.List   `tfsdk:"branch_restrictions"`
	PrRestrictions     types.List   `tfsdk:"pr_restrictions"`
	BuildArguments     types.Map    `tfsdk:"build_arguments"`
	DisableCI          types.Bool   `tfsdk:"disable_ci"`
	CreatedAt          types.String `tfsdk:"created_at"`
}

// buildServicePayload is the body for create (POST) and update (PUT), which
// take the same schema.
type buildServiceRequestPayload struct {
	Name               string                    `json:"name"`
	StageID            *string                   `json:"stageId,omitempty"`
	Infrastructure     *infrastructurePayload    `json:"infrastructure,omitempty"`
	Description        *string                   `json:"description,omitempty"`
	Tags               []string                  `json:"tags"`
	Billing            buildBillingPayload       `json:"billing"`
	VcsData            vcsDataPayload            `json:"vcsData"`
	BuildSource        string                    `json:"buildSource"`
	BuildSettings      buildSettingsPayload      `json:"buildSettings"`
	BuildConfiguration buildConfigurationPayload `json:"buildConfiguration"`
	BuildArguments     map[string]string         `json:"buildArguments"`
	DisabledCI         bool                      `json:"disabledCI"`
}

type buildBillingPayload struct {
	BuildPlan      *string `json:"buildPlan,omitempty"`
	DeploymentPlan *string `json:"deploymentPlan,omitempty"`
}

func (r *buildServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_build_service"
}

func (r *buildServiceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Northflank build service: it builds images from a Git repository, " +
			"without running them. Point a `northflank_deployment_service` or `northflank_job` at it through " +
			"`deployment.build_service` to run what it builds.\n\n" +
			"`build_arguments` values are stored in Terraform state. Treat the state file as a secret.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Identifier of the build service, derived by Northflank from `name`. This is " +
					"what `deployment.build_service.id` takes.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"uid": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Permanent identifier of the service. Unlike `id`, it never changes and is never reused.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"app_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Full identifier Northflank uses for the service.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the service belongs to. Changing this forces a new service.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the service, 3 to 54 characters. Changing this forces a new " +
					"service, since Northflank derives the `id` from it and cannot rename one.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 54),
					stringvalidator.RegexMatches(projectNamePattern,
						"must start with a letter and contain only letters, digits, and single spaces or hyphens between them"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"stage_id":     stageIDAttribute("build service"),
			"architecture": serviceArchitectureAttribute("build service"),
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Short description of the service, up to 200 characters. Removing it from " +
					"configuration keeps the current value.",
				Validators: projectDescriptionValidators(),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"tags": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, nil)),
				MarkdownDescription: "Previously defined Northflank tags to attach to the service.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(secretGroupIdentifierValidators()...),
				},
			},
			"build_plan": buildPlanAttribute(),
			"repository": repositoryAttribute(),
			"build":      buildSettingsAttribute(),
			"branch_restrictions": branchRuleListAttribute("Branches whose commits are built automatically, " +
				"such as `main` or `release/*`. Empty builds no branches automatically."),
			"pr_restrictions": branchRuleListAttribute("Branches whose pull requests are built automatically, " +
				"such as `feature/*`. Empty builds no pull requests automatically."),
			"build_arguments": buildArgumentsAttribute(),
			"disable_ci":      disableCIAttribute(),
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation time, in RFC 3339 format.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

// buildPlanAttribute, buildArgumentsAttribute and disableCIAttribute are
// shared with combined services.
func buildPlanAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "ID of the compute plan builds run on, such as `nf-compute-200`. Northflank picks " +
			"one when this is not set.",
		Validators: []validator.String{
			stringvalidator.LengthBetween(3, 100),
			stringvalidator.RegexMatches(jobPlanPattern, "must be a Northflank plan ID such as nf-compute-200"),
		},
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.UseStateForUnknown(),
		},
	}
}

func buildArgumentsAttribute() schema.MapAttribute {
	return schema.MapAttribute{
		Optional:    true,
		Sensitive:   true,
		ElementType: types.StringType,
		MarkdownDescription: "Build arguments passed to every build, as a map of name to value. Values shared " +
			"by several services belong in a `northflank_secret_group` with `secret_type` set to `arguments`.",
	}
}

func disableCIAttribute() schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional: true,
		Computed: true,
		Default:  booldefault.StaticBool(false),
		MarkdownDescription: "Stop building new commits automatically; builds then only start when triggered. " +
			"Defaults to `false`.",
	}
}

func (r *buildServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *buildServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan buildServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := buildServiceRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := plan.ProjectID.ValueString()
	created, err := createService(ctx, r.client, serviceKindBuild, projectID, body)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank build service",
			fmt.Sprintf("Could not create service %q in project %q: %s", plan.Name.ValueString(), projectID, err),
		)
		return
	}

	tflog.Debug(ctx, "created Northflank build service", map[string]any{"project_id": projectID, "id": created.ID})

	// The service exists from here on, so a failed read-back must still leave
	// state behind or Terraform leaks it.
	plan.ID = types.StringValue(created.ID)
	plan.UID = types.StringValue(created.UID)
	plan.AppID = types.StringValue(created.AppID)
	plan.CreatedAt = timePointerValue(created.CreatedAt)

	if plan.Description.IsUnknown() {
		plan.Description = types.StringNull()
	}

	if plan.BuildPlan.IsUnknown() {
		plan.BuildPlan = types.StringNull()
	}

	service, err := getService(ctx, r.client, projectID, created.ID)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back created Northflank build service",
			fmt.Sprintf("The service was created with ID %q but could not be read: %s. "+
				"It has been saved to state; the remaining attributes are populated on the next refresh.",
				created.ID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyBuildService(ctx, &plan, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *buildServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state buildServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, serviceID := state.ProjectID.ValueString(), state.ID.ValueString()
	service, err := getService(ctx, r.client, projectID, serviceID)

	if err != nil {
		if isNotFound(err) {
			tflog.Debug(ctx, "Northflank service no longer exists, removing from state",
				map[string]any{"project_id": projectID, "id": serviceID})
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Error reading Northflank build service",
			fmt.Sprintf("Could not read service %q in project %q: %s", serviceID, projectID, err),
		)
		return
	}

	if err := checkServiceKind(service, serviceKindBuild); err != nil {
		resp.Diagnostics.AddError("Wrong kind of Northflank service", err.Error())
		return
	}

	resp.Diagnostics.Append(applyBuildService(ctx, &state, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *buildServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state buildServiceResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := buildServiceRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, serviceID := state.ProjectID.ValueString(), state.ID.ValueString()

	if err := updateService(ctx, r.client, serviceKindBuild, projectID, body); err != nil {
		resp.Diagnostics.AddError(
			"Error updating Northflank build service",
			fmt.Sprintf("Could not update service %q in project %q: %s", serviceID, projectID, err),
		)
		return
	}

	service, err := getService(ctx, r.client, projectID, serviceID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading Northflank build service",
			fmt.Sprintf("Service %q was updated but could not be read back: %s", serviceID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyBuildService(ctx, &plan, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *buildServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state buildServiceResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, serviceID := state.ProjectID.ValueString(), state.ID.ValueString()

	if err := deleteService(ctx, r.client, projectID, serviceID); err != nil {
		resp.Diagnostics.AddError(
			"Error deleting Northflank build service",
			fmt.Sprintf("Could not delete service %q in project %q: %s", serviceID, projectID, err),
		)
	}
}

func (r *buildServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, serviceID, err := parseServiceImportID(req.ID, "northflank_build_service")

	if err != nil {
		resp.Diagnostics.AddError("Invalid import identifier", err.Error())
		return
	}

	// Read fills in everything else.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), serviceID)...)
}

func buildServiceRequestBody(ctx context.Context, plan *buildServiceResourceModel) ([]byte, diag.Diagnostics) {
	payload, diags := buildServicePayloadFrom(ctx, plan)

	if diags.HasError() {
		return nil, diags
	}

	body, err := json.Marshal(payload)

	if err != nil {
		diags.AddError("Error building Northflank build service request", err.Error())
	}

	return body, diags
}

func buildServicePayloadFrom(ctx context.Context, plan *buildServiceResourceModel) (*buildServiceRequestPayload, diag.Diagnostics) {
	var diags diag.Diagnostics

	vcs, d := vcsDataPayloadFrom(ctx, plan.Repository, nil)
	diags.Append(d...)

	settings, target, d := buildSettingsPayloadFrom(ctx, plan.Build)
	diags.Append(d...)

	if diags.HasError() {
		return nil, diags
	}

	// Sent explicitly, even when empty, so that removing a rule removes it.
	branches := stringsFromList(ctx, plan.BranchRestrictions, &diags)
	pullRequests := stringsFromList(ctx, plan.PrRestrictions, &diags)

	payload := &buildServiceRequestPayload{
		Name:           plan.Name.ValueString(),
		Description:    knownStringPointer(plan.Description),
		StageID:        knownStringPointer(plan.StageID),
		Infrastructure: infrastructurePayloadFrom(plan.Architecture),
		Tags:           stringsFromSet(ctx, plan.Tags, &diags),
		Billing:        buildBillingPayload{BuildPlan: knownStringPointer(plan.BuildPlan)},
		VcsData:        *vcs,
		BuildSource:    "git",
		BuildSettings:  *settings,
		BuildConfiguration: buildConfigurationPayload{
			DockerfileTarget:   target,
			BranchRestrictions: &branches,
			PrRestrictions:     &pullRequests,
		},
		BuildArguments: map[string]string{},
		DisabledCI:     plan.DisableCI.ValueBool(),
	}

	if !plan.BuildArguments.IsNull() && !plan.BuildArguments.IsUnknown() {
		diags.Append(plan.BuildArguments.ElementsAs(ctx, &payload.BuildArguments, false)...)
	}

	return payload, diags
}

// applyBuildService copies an API response onto the Terraform model.
func applyBuildService(ctx context.Context, model *buildServiceResourceModel, service *serviceRead) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(service.ID)
	model.UID = types.StringValue(service.UID)
	model.AppID = types.StringValue(service.AppID)
	model.Name = types.StringValue(service.Name)
	model.Description = types.StringPointerValue(service.Description)
	model.StageID = types.StringPointerValue(service.StageID)
	model.Architecture = serviceArchitectureValue(service)
	model.BuildPlan = types.StringPointerValue(service.Billing.BuildPlan)
	model.DisableCI = types.BoolValue(service.DisabledCI)
	model.CreatedAt = timePointerValue(service.CreatedAt)

	repository, d := repositoryValue(ctx, model.Repository, service)
	diags.Append(d...)
	model.Repository = repository

	model.BuildArguments = stringMapOrNull(ctx, service.BuildArguments, &diags)

	tags, d := types.SetValueFrom(ctx, types.StringType, nonNil(service.Tags))
	diags.Append(d...)
	model.Tags = tags

	build, d := buildSettingsValue(ctx, model.Build, service)
	diags.Append(d...)
	model.Build = build

	var branches, pullRequests []string

	if c := service.BuildConfiguration; c != nil {
		branches, pullRequests = c.BranchRestrictions, c.PrRestrictions
	}

	model.BranchRestrictions, d = types.ListValueFrom(ctx, types.StringType, nonNil(branches))
	diags.Append(d...)

	model.PrRestrictions, d = types.ListValueFrom(ctx, types.StringType, nonNil(pullRequests))
	diags.Append(d...)

	return diags
}
