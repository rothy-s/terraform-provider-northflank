package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
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
	_ resource.Resource                = (*combinedServiceResource)(nil)
	_ resource.ResourceWithConfigure   = (*combinedServiceResource)(nil)
	_ resource.ResourceWithImportState = (*combinedServiceResource)(nil)
)

type combinedServiceResource struct {
	client *northflank.ClientWithResponses
}

// NewCombinedServiceResource returns the northflank_combined_service resource
// implementation.
func NewCombinedServiceResource() resource.Resource {
	return &combinedServiceResource{}
}

// combinedServiceModel maps the northflank_combined_service schema.
type combinedServiceModel struct {
	ID                 types.String `tfsdk:"id"`
	UID                types.String `tfsdk:"uid"`
	AppID              types.String `tfsdk:"app_id"`
	ProjectID          types.String `tfsdk:"project_id"`
	Name               types.String `tfsdk:"name"`
	Description        types.String `tfsdk:"description"`
	Tags               types.Set    `tfsdk:"tags"`
	DeploymentPlan     types.String `tfsdk:"deployment_plan"`
	BuildPlan          types.String `tfsdk:"build_plan"`
	Instances          types.Int64  `tfsdk:"instances"`
	Repository         types.Object `tfsdk:"repository"`
	Branch             types.String `tfsdk:"branch"`
	Build              types.Object `tfsdk:"build"`
	BuildArguments     types.Map    `tfsdk:"build_arguments"`
	DisableCI          types.Bool   `tfsdk:"disable_ci"`
	Command            types.String `tfsdk:"command"`
	Entrypoint         types.String `tfsdk:"entrypoint"`
	Autoscaling        types.Object `tfsdk:"autoscaling"`
	Ports              types.List   `tfsdk:"ports"`
	HealthChecks       types.List   `tfsdk:"health_checks"`
	RuntimeEnvironment types.Map    `tfsdk:"runtime_environment"`
	CreatedAt          types.String `tfsdk:"created_at"`
}

// combinedServicePayload is the body for create (POST) and update (PUT),
// which take the same schema.
type combinedServicePayload struct {
	Name               string                      `json:"name"`
	Description        *string                     `json:"description,omitempty"`
	Tags               []string                    `json:"tags"`
	Billing            buildBillingPayload         `json:"billing"`
	VcsData            vcsDataPayload              `json:"vcsData"`
	BuildSource        string                      `json:"buildSource"`
	BuildSettings      buildSettingsPayload        `json:"buildSettings"`
	BuildConfiguration buildConfigurationPayload   `json:"buildConfiguration"`
	BuildArguments     map[string]string           `json:"buildArguments"`
	DisabledCI         bool                        `json:"disabledCI"`
	Deployment         combinedDeploymentPayload   `json:"deployment"`
	Autoscaling        *autoscalingPayload         `json:"autoscaling"`
	Ports              []servicePortPayload        `json:"ports"`
	HealthChecks       []serviceHealthCheckPayload `json:"healthChecks"`
	RuntimeEnvironment map[string]string           `json:"runtimeEnvironment"`
}

// combinedDeploymentPayload has no source: a combined service always runs
// what it built itself.
type combinedDeploymentPayload struct {
	Instances int            `json:"instances"`
	Docker    *dockerPayload `json:"docker,omitempty"`
}

func (r *combinedServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_combined_service"
}

func (r *combinedServiceResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	// Ports, health checks and the runtime environment are the deployment
	// service's, so reuse its definitions rather than copying them.
	deploymentSchema := &resource.SchemaResponse{}
	NewDeploymentServiceResource().Schema(ctx, resource.SchemaRequest{}, deploymentSchema)
	shared := deploymentSchema.Schema.Attributes

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Northflank combined service: it builds one branch of a Git repository " +
			"and runs the result, redeploying on every new commit unless `disable_ci` is set. For builds without " +
			"a running service, use `northflank_build_service`.\n\n" +
			"`build_arguments` and `runtime_environment` values are stored in Terraform state. Treat the state " +
			"file as a secret.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identifier of the service, derived by Northflank from `name`.",
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
				MarkdownDescription: "Full identifier Northflank uses for the service's deployment.",
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
			"deployment_plan": shared["deployment_plan"],
			"build_plan":      buildPlanAttribute(),
			"instances": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(serviceDefaultInstances),
				MarkdownDescription: fmt.Sprintf("Number of instances to run. `0` scales the service down "+
					"without deleting it. Defaults to `%d`.", serviceDefaultInstances),
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
			},
			"repository": repositoryAttribute(),
			"branch": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Branch of `repository` to build and deploy.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					stringvalidator.RegexMatches(branchRulePattern, "must be a valid branch name"),
				},
			},
			"build":           buildSettingsAttribute(),
			"build_arguments": buildArgumentsAttribute(),
			"disable_ci":      disableCIAttribute(),
			"command": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Command to run instead of the built image's `CMD`.",
			},
			"entrypoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Entrypoint to run instead of the built image's `ENTRYPOINT`.",
			},
			"autoscaling":         shared["autoscaling"],
			"ports":               shared["ports"],
			"health_checks":       shared["health_checks"],
			"runtime_environment": shared["runtime_environment"],
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

func (r *combinedServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *combinedServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan combinedServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := combinedServiceRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := plan.ProjectID.ValueString()
	created, err := createService(ctx, r.client, serviceKindCombined, projectID, body)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank combined service",
			fmt.Sprintf("Could not create service %q in project %q: %s", plan.Name.ValueString(), projectID, err),
		)
		return
	}

	tflog.Debug(ctx, "created Northflank combined service", map[string]any{"project_id": projectID, "id": created.ID})

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
			"Error reading back created Northflank combined service",
			fmt.Sprintf("The service was created with ID %q but could not be read: %s. "+
				"It has been saved to state; the remaining attributes are populated on the next refresh.",
				created.ID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyCombinedService(ctx, &plan, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *combinedServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state combinedServiceModel
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
			"Error reading Northflank combined service",
			fmt.Sprintf("Could not read service %q in project %q: %s", serviceID, projectID, err),
		)
		return
	}

	if err := checkServiceKind(service, serviceKindCombined); err != nil {
		resp.Diagnostics.AddError("Wrong kind of Northflank service", err.Error())
		return
	}

	resp.Diagnostics.Append(applyCombinedService(ctx, &state, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *combinedServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state combinedServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := combinedServiceRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, serviceID := state.ProjectID.ValueString(), state.ID.ValueString()

	if err := updateService(ctx, r.client, serviceKindCombined, projectID, body); err != nil {
		resp.Diagnostics.AddError(
			"Error updating Northflank combined service",
			fmt.Sprintf("Could not update service %q in project %q: %s", serviceID, projectID, err),
		)
		return
	}

	service, err := getService(ctx, r.client, projectID, serviceID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading Northflank combined service",
			fmt.Sprintf("Service %q was updated but could not be read back: %s", serviceID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyCombinedService(ctx, &plan, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *combinedServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state combinedServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, serviceID := state.ProjectID.ValueString(), state.ID.ValueString()

	if err := deleteService(ctx, r.client, projectID, serviceID); err != nil {
		resp.Diagnostics.AddError(
			"Error deleting Northflank combined service",
			fmt.Sprintf("Could not delete service %q in project %q: %s", serviceID, projectID, err),
		)
	}
}

func (r *combinedServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, serviceID, err := parseServiceImportID(req.ID, "northflank_combined_service")

	if err != nil {
		resp.Diagnostics.AddError("Invalid import identifier", err.Error())
		return
	}

	// Read fills in everything else.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), serviceID)...)
}

func combinedServiceRequestBody(ctx context.Context, plan *combinedServiceModel) ([]byte, diag.Diagnostics) {
	payload, diags := combinedServicePayloadFrom(ctx, plan)

	if diags.HasError() {
		return nil, diags
	}

	body, err := json.Marshal(payload)

	if err != nil {
		diags.AddError("Error building Northflank combined service request", err.Error())
	}

	return body, diags
}

func combinedServicePayloadFrom(ctx context.Context, plan *combinedServiceModel) (*combinedServicePayload, diag.Diagnostics) {
	var diags diag.Diagnostics

	vcs, d := vcsDataPayloadFrom(ctx, plan.Repository, plan.Branch.ValueStringPointer())
	diags.Append(d...)

	settings, target, d := buildSettingsPayloadFrom(ctx, plan.Build)
	diags.Append(d...)

	ports, d := portsPayload(ctx, plan.Ports)
	diags.Append(d...)

	healthChecks, d := healthChecksPayload(ctx, plan.HealthChecks)
	diags.Append(d...)

	autoscaling, d := autoscalingPayloadFrom(ctx, plan.Autoscaling, plan.Instances.ValueInt64())
	diags.Append(d...)

	if diags.HasError() {
		return nil, diags
	}

	deploymentPlan := plan.DeploymentPlan.ValueString()

	payload := &combinedServicePayload{
		Name:        plan.Name.ValueString(),
		Description: knownStringPointer(plan.Description),
		Tags:        stringsFromSet(ctx, plan.Tags, &diags),
		Billing: buildBillingPayload{
			DeploymentPlan: &deploymentPlan,
			BuildPlan:      knownStringPointer(plan.BuildPlan),
		},
		VcsData:            *vcs,
		BuildSource:        "git",
		BuildSettings:      *settings,
		BuildConfiguration: buildConfigurationPayload{DockerfileTarget: target},
		BuildArguments:     map[string]string{},
		DisabledCI:         plan.DisableCI.ValueBool(),
		Deployment: combinedDeploymentPayload{
			Instances: int(plan.Instances.ValueInt64()),
			Docker:    dockerOverride(knownStringPointer(plan.Command), knownStringPointer(plan.Entrypoint)),
		},
		Autoscaling:        autoscaling,
		Ports:              ports,
		HealthChecks:       healthChecks,
		RuntimeEnvironment: map[string]string{},
	}

	if !plan.BuildArguments.IsNull() && !plan.BuildArguments.IsUnknown() {
		diags.Append(plan.BuildArguments.ElementsAs(ctx, &payload.BuildArguments, false)...)
	}

	if !plan.RuntimeEnvironment.IsNull() && !plan.RuntimeEnvironment.IsUnknown() {
		diags.Append(plan.RuntimeEnvironment.ElementsAs(ctx, &payload.RuntimeEnvironment, false)...)
	}

	return payload, diags
}

// applyCombinedService copies an API response onto the Terraform model.
func applyCombinedService(ctx context.Context, model *combinedServiceModel, service *serviceRead) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(service.ID)
	model.UID = types.StringValue(service.UID)
	model.AppID = types.StringValue(service.AppID)
	model.Name = types.StringValue(service.Name)
	model.Description = types.StringPointerValue(service.Description)
	model.DeploymentPlan = types.StringValue(service.Billing.DeploymentPlan)
	model.BuildPlan = types.StringPointerValue(service.Billing.BuildPlan)
	model.DisableCI = types.BoolValue(service.DisabledCI)
	model.CreatedAt = timePointerValue(service.CreatedAt)
	model.Command = types.StringPointerValue(service.command())
	model.Entrypoint = types.StringPointerValue(service.entrypoint())
	model.BuildArguments = stringMapOrNull(ctx, service.BuildArguments, &diags)
	model.RuntimeEnvironment = stringMapOrNull(ctx, service.RuntimeEnvironment, &diags)

	autoscaling, d := autoscalingValue(ctx, service.Autoscaling)
	diags.Append(d...)
	model.Autoscaling = autoscaling

	// While autoscaling is on, the autoscaler owns the instance count, so the
	// reported value would show as drift on every plan.
	if service.Deployment != nil && autoscaling.IsNull() {
		model.Instances = types.Int64Value(int64(service.Deployment.Instances))
	}

	if service.VcsData != nil {
		model.Branch = types.StringPointerValue(service.VcsData.ProjectBranch)
	}

	repository, d := repositoryValue(ctx, model.Repository, service)
	diags.Append(d...)
	model.Repository = repository

	build, d := buildSettingsValue(ctx, service)
	diags.Append(d...)
	model.Build = build

	tags, d := types.SetValueFrom(ctx, types.StringType, nonNil(service.Tags))
	diags.Append(d...)
	model.Tags = tags

	ports, d := portsValue(ctx, service.Ports)
	diags.Append(d...)
	model.Ports = ports

	healthChecks, d := healthChecksValue(ctx, service.HealthChecks)
	diags.Append(d...)
	model.HealthChecks = healthChecks

	return diags
}
