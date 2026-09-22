package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
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
	_ resource.Resource                = (*deploymentServiceResource)(nil)
	_ resource.ResourceWithConfigure   = (*deploymentServiceResource)(nil)
	_ resource.ResourceWithImportState = (*deploymentServiceResource)(nil)
)

type deploymentServiceResource struct {
	client *northflank.ClientWithResponses
}

// NewDeploymentServiceResource returns the northflank_deployment_service
// resource implementation.
func NewDeploymentServiceResource() resource.Resource {
	return &deploymentServiceResource{}
}

// deploymentServiceModel maps the northflank_deployment_service schema.
type deploymentServiceModel struct {
	ID                 types.String `tfsdk:"id"`
	UID                types.String `tfsdk:"uid"`
	AppID              types.String `tfsdk:"app_id"`
	ProjectID          types.String `tfsdk:"project_id"`
	Name               types.String `tfsdk:"name"`
	Description        types.String `tfsdk:"description"`
	Tags               types.Set    `tfsdk:"tags"`
	DeploymentPlan     types.String `tfsdk:"deployment_plan"`
	Instances          types.Int64  `tfsdk:"instances"`
	Deployment         types.Object `tfsdk:"deployment"`
	Autoscaling        types.Object `tfsdk:"autoscaling"`
	Ports              types.List   `tfsdk:"ports"`
	HealthChecks       types.List   `tfsdk:"health_checks"`
	RuntimeEnvironment types.Map    `tfsdk:"runtime_environment"`
	CreatedAt          types.String `tfsdk:"created_at"`
}

// deploymentServicePayload is the body for create (POST) and update (PUT),
// which take the same schema.
type deploymentServicePayload struct {
	Name               string                      `json:"name"`
	Description        *string                     `json:"description,omitempty"`
	Tags               []string                    `json:"tags"`
	Billing            jobBillingPayload           `json:"billing"`
	Deployment         serviceDeploymentPayload    `json:"deployment"`
	Autoscaling        *autoscalingPayload         `json:"autoscaling"`
	Ports              []servicePortPayload        `json:"ports"`
	HealthChecks       []serviceHealthCheckPayload `json:"healthChecks"`
	RuntimeEnvironment map[string]string           `json:"runtimeEnvironment"`
}

// serviceDeploymentPayload is the shared deployment block plus the instance
// count, which only services have.
type serviceDeploymentPayload struct {
	deploymentPayload
	Instances int `json:"instances"`
}

func (r *deploymentServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_deployment_service"
}

func (r *deploymentServiceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	portType := types.ObjectType{AttrTypes: servicePortAttrTypes}
	healthCheckType := types.ObjectType{AttrTypes: serviceHealthCheckAttrTypes}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Northflank deployment service: a long-running workload that runs an " +
			"external image or the output of a Northflank build service. For a service that also builds from " +
			"Git, Northflank has combined services, which this provider does not manage yet.\n\n" +
			"`runtime_environment` values are stored in Terraform state. Treat the state file as a secret.",
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
			"deployment_plan": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the compute plan each instance uses, such as `nf-compute-20`.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 100),
					stringvalidator.RegexMatches(jobPlanPattern, "must be a Northflank plan ID such as nf-compute-20"),
				},
			},
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
			"deployment": deploymentSourceAttribute("What the service runs. Set exactly one of `external_image` " +
				"or `build_service`."),
			"autoscaling": autoscalingAttribute(),
			"ports": schema.ListNestedAttribute{
				Optional:            true,
				Computed:            true,
				Default:             listdefault.StaticValue(types.ListValueMust(portType, nil)),
				MarkdownDescription: "Ports the service listens on, and whether each is reachable from the internet.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "Name identifying the port, up to 8 characters.",
							Validators: []validator.String{
								stringvalidator.LengthBetween(1, 8),
								stringvalidator.RegexMatches(servicePortNamePattern,
									"must start with a letter and contain only letters, digits and single hyphens"),
							},
						},
						"internal_port": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Port number the container listens on.",
							Validators: []validator.Int64{
								int64validator.Between(1, 65535),
							},
						},
						"protocol": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "`HTTP`, `HTTP/2`, `TCP` or `UDP`. Only HTTP ports can be public.",
							Validators: []validator.String{
								stringvalidator.OneOf(servicePortProtocols...),
							},
						},
						"public": schema.BoolAttribute{
							Optional:            true,
							Computed:            true,
							Default:             booldefault.StaticBool(false),
							MarkdownDescription: "Whether the port is exposed to the internet. Defaults to `false`.",
						},
						"domains": schema.SetAttribute{
							Optional:            true,
							Computed:            true,
							ElementType:         types.StringType,
							Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, nil)),
							MarkdownDescription: "Custom domains routed to this port. Each must already be verified in Northflank.",
						},
					},
				},
			},
			"health_checks": schema.ListNestedAttribute{
				Optional:            true,
				Computed:            true,
				Default:             listdefault.StaticValue(types.ListValueMust(healthCheckType, nil)),
				MarkdownDescription: "Probes Northflank uses to decide whether an instance is started, alive and ready.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"type": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "`livenessProbe`, `readinessProbe` or `startupProbe`.",
							Validators: []validator.String{
								stringvalidator.OneOf(serviceHealthCheckTypes...),
							},
						},
						"protocol": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "`HTTP` (needs `path` and `port`), `TCP` (needs `port`) or " +
								"`CMD` (needs `cmd`).",
							Validators: []validator.String{
								stringvalidator.OneOf(serviceHealthCheckProtocols...),
							},
						},
						"path": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "HTTP path to request, such as `/healthz`.",
						},
						"port": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "Port to check.",
							Validators: []validator.Int64{
								int64validator.Between(1, 65535),
							},
						},
						"cmd": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "Command to run inside the container.",
						},
						"initial_delay_seconds": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Seconds to wait before the first check, 1 to 1800.",
							Validators: []validator.Int64{
								int64validator.Between(1, 1800),
							},
						},
						"period_seconds": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Seconds between checks, 10 to 600.",
							Validators: []validator.Int64{
								int64validator.Between(10, 600),
							},
						},
						"timeout_seconds": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Seconds to wait for a response, 1 to 60.",
							Validators: []validator.Int64{
								int64validator.Between(1, 60),
							},
						},
						"failure_threshold": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Failed checks in a row before the instance counts as failing, 1 to 255.",
							Validators: []validator.Int64{
								int64validator.Between(1, 255),
							},
						},
						"success_threshold": schema.Int64Attribute{
							Optional:            true,
							Computed:            true,
							Default:             int64default.StaticInt64(1),
							MarkdownDescription: "Successful checks in a row before the instance counts as healthy. Defaults to `1`.",
							Validators: []validator.Int64{
								int64validator.Between(1, 255),
							},
						},
					},
				},
			},
			"runtime_environment": schema.MapAttribute{
				Optional:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "Environment variables set on every instance, as a map of name to value. " +
					"Values shared by several services belong in a `northflank_secret_group`.",
			},
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

func (r *deploymentServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *deploymentServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan deploymentServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := deploymentServiceRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := plan.ProjectID.ValueString()
	createdService, err := createService(ctx, r.client, serviceKindDeployment, projectID, body)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank deployment service",
			fmt.Sprintf("Could not create service %q in project %q: %s", plan.Name.ValueString(), projectID, err),
		)
		return
	}

	serviceID := createdService.ID
	tflog.Debug(ctx, "created Northflank deployment service", map[string]any{"project_id": projectID, "id": serviceID})

	// The service exists from here on, so a failed read-back must still leave
	// state behind or Terraform leaks it.
	plan.ID = types.StringValue(serviceID)
	plan.UID = types.StringValue(createdService.UID)
	plan.AppID = types.StringValue(createdService.AppID)
	plan.CreatedAt = timePointerValue(createdService.CreatedAt)

	if plan.Description.IsUnknown() {
		plan.Description = types.StringNull()
	}

	service, err := getService(ctx, r.client, projectID, serviceID)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back created Northflank deployment service",
			fmt.Sprintf("The service was created with ID %q but could not be read: %s. "+
				"It has been saved to state; the remaining attributes are populated on the next refresh.",
				serviceID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyDeploymentService(ctx, &plan, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *deploymentServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state deploymentServiceModel
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
			"Error reading Northflank deployment service",
			fmt.Sprintf("Could not read service %q in project %q: %s", serviceID, projectID, err),
		)
		return
	}

	if err := checkServiceKind(service, serviceKindDeployment); err != nil {
		resp.Diagnostics.AddError("Wrong kind of Northflank service", err.Error())
		return
	}

	resp.Diagnostics.Append(applyDeploymentService(ctx, &state, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *deploymentServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state deploymentServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// PUT takes the whole service and is idempotent, so it is safe to retry,
	// and lists such as ports are replaced rather than merged.
	body, diags := deploymentServiceRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, serviceID := state.ProjectID.ValueString(), state.ID.ValueString()

	if err := updateService(ctx, r.client, serviceKindDeployment, projectID, body); err != nil {
		resp.Diagnostics.AddError(
			"Error updating Northflank deployment service",
			fmt.Sprintf("Could not update service %q in project %q: %s", serviceID, projectID, err),
		)
		return
	}

	service, err := getService(ctx, r.client, projectID, serviceID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading Northflank deployment service",
			fmt.Sprintf("Service %q was updated but could not be read back: %s", serviceID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyDeploymentService(ctx, &plan, service)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *deploymentServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state deploymentServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, serviceID := state.ProjectID.ValueString(), state.ID.ValueString()

	if err := deleteService(ctx, r.client, projectID, serviceID); err != nil {
		resp.Diagnostics.AddError(
			"Error deleting Northflank deployment service",
			fmt.Sprintf("Could not delete service %q in project %q: %s", serviceID, projectID, err),
		)
	}
}

func (r *deploymentServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, serviceID, err := parseServiceImportID(req.ID, "northflank_deployment_service")

	if err != nil {
		resp.Diagnostics.AddError("Invalid import identifier", err.Error())
		return
	}

	// Read fills in everything else.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), serviceID)...)
}

func deploymentServiceRequestBody(ctx context.Context, plan *deploymentServiceModel) ([]byte, diag.Diagnostics) {
	payload, diags := deploymentServicePayloadFrom(ctx, plan)

	if diags.HasError() {
		return nil, diags
	}

	body, err := json.Marshal(payload)

	if err != nil {
		diags.AddError("Error building Northflank deployment service request", err.Error())
	}

	return body, diags
}

func deploymentServicePayloadFrom(ctx context.Context, plan *deploymentServiceModel) (*deploymentServicePayload, diag.Diagnostics) {
	var diags diag.Diagnostics

	deployment, d := deploymentPayloadFrom(ctx, plan.Deployment)
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

	payload := &deploymentServicePayload{
		Name:        plan.Name.ValueString(),
		Description: knownStringPointer(plan.Description),
		Tags:        stringsFromSet(ctx, plan.Tags, &diags),
		Billing:     jobBillingPayload{DeploymentPlan: plan.DeploymentPlan.ValueString()},
		Deployment: serviceDeploymentPayload{
			deploymentPayload: *deployment,
			Instances:         int(plan.Instances.ValueInt64()),
		},
		Autoscaling:        autoscaling,
		Ports:              ports,
		HealthChecks:       healthChecks,
		RuntimeEnvironment: map[string]string{},
	}

	if !plan.RuntimeEnvironment.IsNull() && !plan.RuntimeEnvironment.IsUnknown() {
		diags.Append(plan.RuntimeEnvironment.ElementsAs(ctx, &payload.RuntimeEnvironment, false)...)
	}

	return payload, diags
}

// applyDeploymentService copies an API response onto the Terraform model.
func applyDeploymentService(ctx context.Context, model *deploymentServiceModel, service *serviceRead) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(service.ID)
	model.UID = types.StringValue(service.UID)
	model.AppID = types.StringValue(service.AppID)
	model.Name = types.StringValue(service.Name)
	model.Description = types.StringPointerValue(service.Description)
	model.DeploymentPlan = types.StringValue(service.Billing.DeploymentPlan)
	model.CreatedAt = timePointerValue(service.CreatedAt)

	tags, d := types.SetValueFrom(ctx, types.StringType, nonNil(service.Tags))
	diags.Append(d...)
	model.Tags = tags

	autoscaling, d := autoscalingValue(ctx, service.Autoscaling)
	diags.Append(d...)
	model.Autoscaling = autoscaling

	// While autoscaling is on, the autoscaler owns the instance count, so the
	// reported value would show as drift on every plan.
	if service.Deployment != nil && autoscaling.IsNull() {
		model.Instances = types.Int64Value(int64(service.Deployment.Instances))
	}

	deployment, d := serviceDeploymentValue(ctx, model.Deployment, service)
	diags.Append(d...)
	model.Deployment = deployment

	ports, d := portsValue(ctx, service.Ports)
	diags.Append(d...)
	model.Ports = ports

	healthChecks, d := healthChecksValue(ctx, service.HealthChecks)
	diags.Append(d...)
	model.HealthChecks = healthChecks

	if len(service.RuntimeEnvironment) == 0 {
		model.RuntimeEnvironment = types.MapNull(types.StringType)
	} else {
		env, d := types.MapValueFrom(ctx, types.StringType, service.RuntimeEnvironment)
		diags.Append(d...)
		model.RuntimeEnvironment = env
	}

	return diags
}

// serviceDeploymentValue builds the deployment block from what the API
// reports. Unlike jobs, services report their deployment source, so drift in
// it is detected. Two values are taken from the current model instead:
// registry credentials, which the API may omit, and a build_sha of "latest",
// which the API may report as the commit it resolved to.
func serviceDeploymentValue(ctx context.Context, current types.Object, service *serviceRead) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics

	var prior deploymentSourceModel
	priorImage := externalImageModel{CredentialsID: types.StringNull()}
	priorBuild := buildServiceModel{BuildSHA: types.StringNull()}

	if !current.IsNull() && !current.IsUnknown() {
		diags.Append(current.As(ctx, &prior, objectAsOptions)...)

		if !prior.ExternalImage.IsNull() && !prior.ExternalImage.IsUnknown() {
			diags.Append(prior.ExternalImage.As(ctx, &priorImage, objectAsOptions)...)
		}

		if !prior.BuildService.IsNull() && !prior.BuildService.IsUnknown() {
			diags.Append(prior.BuildService.As(ctx, &priorBuild, objectAsOptions)...)
		}
	}

	external := types.ObjectNull(externalImageAttrTypes)
	build := types.ObjectNull(buildServiceAttrTypes)

	if d := service.Deployment; d != nil && d.External != nil {
		credentials := types.StringPointerValue(d.External.Credentials)

		if credentials.IsNull() {
			credentials = priorImage.CredentialsID
		}

		external = types.ObjectValueMust(externalImageAttrTypes, map[string]attr.Value{
			"image_path":     types.StringValue(d.External.ImagePath),
			"credentials_id": nullIfUnknown(credentials),
		})
	}

	if d := service.Deployment; d != nil && d.Internal != nil {
		sha := types.StringPointerValue(d.Internal.BuildSHA)

		if priorBuild.BuildSHA.ValueString() == "latest" || sha.IsNull() {
			sha = priorBuild.BuildSHA
		}

		if sha.IsNull() || sha.IsUnknown() {
			sha = types.StringValue("latest")
		}

		build = types.ObjectValueMust(buildServiceAttrTypes, map[string]attr.Value{
			"id":        types.StringValue(d.Internal.ID),
			"branch":    types.StringPointerValue(d.Internal.Branch),
			"build_sha": sha,
		})
	}

	value, d := types.ObjectValueFrom(ctx, deploymentSourceAttrTypes, deploymentSourceModel{
		ExternalImage: external,
		BuildService:  build,
		Command:       types.StringPointerValue(service.command()),
		Entrypoint:    types.StringPointerValue(service.entrypoint()),
	})
	diags.Append(d...)

	return value, diags
}

func nullIfUnknown(v types.String) types.String {
	if v.IsUnknown() {
		return types.StringNull()
	}

	return v
}
