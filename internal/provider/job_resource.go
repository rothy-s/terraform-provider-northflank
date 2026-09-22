package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/objectvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
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
	_ resource.Resource                = (*jobResource)(nil)
	_ resource.ResourceWithConfigure   = (*jobResource)(nil)
	_ resource.ResourceWithImportState = (*jobResource)(nil)
)

type jobResource struct {
	client *northflank.ClientWithResponses
}

// NewJobResource returns the northflank_job resource implementation.
func NewJobResource() resource.Resource {
	return &jobResource{}
}

// jobResourceModel maps the northflank_job schema.
type jobResourceModel struct {
	ID                    types.String `tfsdk:"id"`
	UID                   types.String `tfsdk:"uid"`
	AppID                 types.String `tfsdk:"app_id"`
	ProjectID             types.String `tfsdk:"project_id"`
	Name                  types.String `tfsdk:"name"`
	Description           types.String `tfsdk:"description"`
	Tags                  types.Set    `tfsdk:"tags"`
	DeploymentPlan        types.String `tfsdk:"deployment_plan"`
	Deployment            types.Object `tfsdk:"deployment"`
	Cron                  types.Object `tfsdk:"cron"`
	BackoffLimit          types.Int64  `tfsdk:"backoff_limit"`
	ActiveDeadlineSeconds types.Int64  `tfsdk:"active_deadline_seconds"`
	RuntimeEnvironment    types.Map    `tfsdk:"runtime_environment"`
	JobType               types.String `tfsdk:"job_type"`
	Suspended             types.Bool   `tfsdk:"suspended"`
	CreatedAt             types.String `tfsdk:"created_at"`
}

type jobDeploymentModel struct {
	ExternalImage types.Object `tfsdk:"external_image"`
	BuildService  types.Object `tfsdk:"build_service"`
	Command       types.String `tfsdk:"command"`
	Entrypoint    types.String `tfsdk:"entrypoint"`
}

type jobExternalImageModel struct {
	ImagePath     types.String `tfsdk:"image_path"`
	CredentialsID types.String `tfsdk:"credentials_id"`
}

type jobBuildServiceModel struct {
	ID       types.String `tfsdk:"id"`
	Branch   types.String `tfsdk:"branch"`
	BuildSHA types.String `tfsdk:"build_sha"`
}

type jobCronModel struct {
	Schedule          types.String `tfsdk:"schedule"`
	ConcurrencyPolicy types.String `tfsdk:"concurrency_policy"`
}

var (
	jobExternalImageAttrTypes = map[string]attr.Type{
		"image_path":     types.StringType,
		"credentials_id": types.StringType,
	}

	jobBuildServiceAttrTypes = map[string]attr.Type{
		"id":        types.StringType,
		"branch":    types.StringType,
		"build_sha": types.StringType,
	}

	jobDeploymentAttrTypes = map[string]attr.Type{
		"external_image": types.ObjectType{AttrTypes: jobExternalImageAttrTypes},
		"build_service":  types.ObjectType{AttrTypes: jobBuildServiceAttrTypes},
		"command":        types.StringType,
		"entrypoint":     types.StringType,
	}

	jobCronAttrTypes = map[string]attr.Type{
		"schedule":           types.StringType,
		"concurrency_policy": types.StringType,
	}
)

func (r *jobResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_job"
}

func (r *jobResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Northflank job: a container that runs to completion, either on a `cron` " +
			"schedule or when triggered manually. The job deploys an external image or the output of a " +
			"Northflank build service.\n\n" +
			"`runtime_environment` values are stored in Terraform state. Treat the state file as a secret.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identifier of the job, derived by Northflank from `name`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"uid": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Permanent identifier of the job. Unlike `id`, it never changes and is never reused.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"app_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Full identifier Northflank uses for the job's deployment.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the job belongs to. Changing this forces a new job.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the job, 3 to 52 characters. Changing this forces a new job, since " +
					"Northflank derives the `id` from it and cannot rename one.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 52),
					stringvalidator.RegexMatches(jobNamePattern,
						"must start with a letter and contain only letters, digits, and single spaces or hyphens between them"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Short description of the job, up to 200 characters. Removing it from " +
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
				MarkdownDescription: "Previously defined Northflank tags to attach to the job.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(secretGroupIdentifierValidators()...),
				},
			},
			"deployment_plan": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the compute plan each job run uses, such as `nf-compute-20`.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 100),
					stringvalidator.RegexMatches(jobPlanPattern, "must be a Northflank plan ID such as nf-compute-20"),
				},
			},
			"deployment": schema.SingleNestedAttribute{
				Required: true,
				MarkdownDescription: "What the job runs. Set exactly one of `external_image` or `build_service`.\n\n" +
					"The API does not report the image or build service back, so a change made outside Terraform " +
					"is not detected, and an imported job shows a difference until the next apply.",
				Attributes: map[string]schema.Attribute{
					"external_image": schema.SingleNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Run an image from a container registry.",
						Validators: []validator.Object{
							objectvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("build_service")),
						},
						Attributes: map[string]schema.Attribute{
							"image_path": schema.StringAttribute{
								Required: true,
								MarkdownDescription: "Image to run, such as `nginx:latest` or " +
									"`ghcr.io/org/image:tag`. Images outside Docker Hub need the registry host.",
								Validators: []validator.String{
									stringvalidator.LengthAtLeast(1),
								},
							},
							"credentials_id": schema.StringAttribute{
								Optional:            true,
								MarkdownDescription: "ID of saved registry credentials, for a private image.",
							},
						},
					},
					"build_service": schema.SingleNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Run the output of a Northflank build service.",
						Attributes: map[string]schema.Attribute{
							"id": schema.StringAttribute{
								Required: true,
								MarkdownDescription: "ID of the build service: `my-builder` in the same project, or " +
									"`other-project/my-builder` in another one.",
								Validators: []validator.String{
									stringvalidator.RegexMatches(jobBuildServicePattern,
										"must be a build service ID, optionally prefixed with its project ID and a slash"),
								},
							},
							"branch": schema.StringAttribute{
								Optional:            true,
								MarkdownDescription: "Branch whose builds to deploy.",
							},
							"build_sha": schema.StringAttribute{
								Optional: true,
								Computed: true,
								Default:  stringdefault.StaticString("latest"),
								MarkdownDescription: "Commit to deploy: a full 40-character SHA, or `latest` for the " +
									"most recent build. Defaults to `latest`.",
								Validators: []validator.String{
									stringvalidator.RegexMatches(jobBuildSHAPattern, "must be a 40-character commit SHA or latest"),
								},
							},
						},
					},
					"command": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "Command to run instead of the image's `CMD`.",
					},
					"entrypoint": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "Entrypoint to run instead of the image's `ENTRYPOINT`.",
					},
				},
			},
			"cron": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Run the job on a schedule. Without this block the job only runs when " +
					"triggered. Adding or removing the block forces a new job, since it changes the job type.",
				PlanModifiers: []planmodifier.Object{
					objectplanmodifier.RequiresReplaceIf(
						func(_ context.Context, req planmodifier.ObjectRequest, resp *objectplanmodifier.RequiresReplaceIfFuncResponse) {
							resp.RequiresReplace = req.StateValue.IsNull() != req.PlanValue.IsNull()
						},
						"Switching between a cron and a manual job forces a new job.",
						"Switching between a cron and a manual job forces a new job.",
					),
				},
				Attributes: map[string]schema.Attribute{
					"schedule": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Cron expression such as `30 8 * * *`, or a shorthand such as `@daily`.",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"concurrency_policy": schema.StringAttribute{
						Optional: true,
						Computed: true,
						Default:  stringdefault.StaticString(jobDefaultConcurrencyPolicy),
						MarkdownDescription: "What happens when a run is due while another is still going: " +
							"`allow` runs both, `forbid` skips the new one, `replace` stops the old one. Defaults " +
							"to `" + jobDefaultConcurrencyPolicy + "`.",
						Validators: []validator.String{
							stringvalidator.OneOf(jobConcurrencyPolicies...),
						},
					},
				},
			},
			"backoff_limit": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(jobDefaultBackoffLimit),
				MarkdownDescription: fmt.Sprintf("Number of times a failed run is retried before it is marked "+
					"as failed. Defaults to `%d`.", jobDefaultBackoffLimit),
				Validators: []validator.Int64{
					int64validator.AtLeast(0),
				},
			},
			"active_deadline_seconds": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Longest a run may take, in seconds, before it is marked as failed. " +
					"Northflank applies its own limit when this is not set.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"runtime_environment": schema.MapAttribute{
				Optional:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "Environment variables set on every run, as a map of name to value. The " +
					"API does not report these back, so a change made outside Terraform is not detected. Values " +
					"shared by several jobs or services belong in a `northflank_secret_group`.",
			},
			"job_type": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`cron` when a schedule is set, otherwise `manual`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"suspended": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether a cron job's schedule is currently suspended.",
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

func (r *jobResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *jobResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan jobResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := jobRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := plan.ProjectID.ValueString()

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostProjectsJobsWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))

	if err != nil {
		resp.Diagnostics.AddError("Error creating Northflank job", err.Error())
		return
	}

	if err := northflank.CheckResponse(created); err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank job",
			fmt.Sprintf("Could not create job %q in project %q: %s", plan.Name.ValueString(), projectID, err),
		)
		return
	}

	if created.JSON200 == nil {
		resp.Diagnostics.AddError("Error creating Northflank job", "The API returned a successful status with no job body.")
		return
	}

	jobID := created.JSON200.Data.Id
	tflog.Debug(ctx, "created Northflank job", map[string]any{"project_id": projectID, "id": jobID})

	// The job exists from here on, so a failed read-back must still leave
	// state behind or Terraform leaks it. Fill every computed attribute that
	// is still unknown so that the state is writable.
	plan.ID = types.StringValue(jobID)
	plan.AppID = types.StringValue(created.JSON200.Data.AppId)
	plan.JobType = types.StringValue(string(created.JSON200.Data.JobType))
	plan.UID = types.StringNull()
	plan.CreatedAt = types.StringNull()
	plan.Suspended = types.BoolValue(false)

	if plan.Description.IsUnknown() {
		plan.Description = types.StringNull()
	}

	if plan.ActiveDeadlineSeconds.IsUnknown() {
		plan.ActiveDeadlineSeconds = types.Int64Null()
	}

	job, err := getJob(ctx, r.client, projectID, jobID)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back created Northflank job",
			fmt.Sprintf("The job was created with ID %q but could not be read: %s. "+
				"It has been saved to state; the remaining attributes are populated on the next refresh.",
				jobID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyJob(ctx, &plan, job)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *jobResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state jobResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, jobID := state.ProjectID.ValueString(), state.ID.ValueString()
	job, err := getJob(ctx, r.client, projectID, jobID)

	if err != nil {
		if isNotFound(err) {
			tflog.Debug(ctx, "Northflank job no longer exists, removing from state",
				map[string]any{"project_id": projectID, "id": jobID})
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Error reading Northflank job",
			fmt.Sprintf("Could not read job %q in project %q: %s", jobID, projectID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyJob(ctx, &state, job)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *jobResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state jobResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// PUT takes the whole job, including what it deploys -- which PATCH
	// cannot change -- and is idempotent, so it is safe to retry.
	body, diags := jobRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, jobID := state.ProjectID.ValueString(), state.ID.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		updated, err := r.client.PutProjectsJobsWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))

		if err != nil {
			return err
		}

		return northflank.CheckResponse(updated)
	})

	if err != nil {
		resp.Diagnostics.AddError(
			"Error updating Northflank job",
			fmt.Sprintf("Could not update job %q in project %q: %s", jobID, projectID, err),
		)
		return
	}

	job, err := getJob(ctx, r.client, projectID, jobID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading Northflank job",
			fmt.Sprintf("Job %q was updated but could not be read back: %s", jobID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyJob(ctx, &plan, job)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *jobResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state jobResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, jobID := state.ProjectID.ValueString(), state.ID.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteProjectsJobsJobidWithResponse(ctx, projectID, jobID)

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
		"Error deleting Northflank job",
		fmt.Sprintf("Could not delete job %q in project %q: %s", jobID, projectID, err),
	)
}

func (r *jobResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, jobID, ok := strings.Cut(req.ID, "/")

	if !ok || projectID == "" || jobID == "" || strings.Contains(jobID, "/") {
		resp.Diagnostics.AddError(
			"Invalid import identifier",
			fmt.Sprintf("Expected <project_id>/<job_id>, got %q. Import with: "+
				"terraform import northflank_job.example my-project/my-job", req.ID),
		)
		return
	}

	// Read fills in everything the API reports; what it does not report
	// (deployment source, runtime environment) comes from the next apply.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), jobID)...)
}

// jobRequestBody builds the create and update body from the plan.
func jobRequestBody(ctx context.Context, plan *jobResourceModel) ([]byte, diag.Diagnostics) {
	payload, diags := jobPayloadFrom(ctx, plan)

	if diags.HasError() {
		return nil, diags
	}

	body, err := json.Marshal(payload)

	if err != nil {
		diags.AddError("Error building Northflank job request", err.Error())
	}

	return body, diags
}

func jobPayloadFrom(ctx context.Context, plan *jobResourceModel) (*jobPayload, diag.Diagnostics) {
	var diags diag.Diagnostics

	payload := &jobPayload{
		Name:               plan.Name.ValueString(),
		Description:        knownStringPointer(plan.Description),
		Tags:               stringsFromSet(ctx, plan.Tags, &diags),
		Billing:            jobBillingPayload{DeploymentPlan: plan.DeploymentPlan.ValueString()},
		Settings:           jobSettingsPayload{BackoffLimit: int(plan.BackoffLimit.ValueInt64())},
		RuntimeEnvironment: map[string]string{},
	}

	if v := plan.ActiveDeadlineSeconds; !v.IsNull() && !v.IsUnknown() {
		seconds := int(v.ValueInt64())
		payload.Settings.ActiveDeadlineSeconds = &seconds
	}

	if !plan.Cron.IsNull() && !plan.Cron.IsUnknown() {
		var cron jobCronModel
		diags.Append(plan.Cron.As(ctx, &cron, objectAsOptions)...)

		payload.Settings.Cron = &jobCronPayload{
			Schedule:          cron.Schedule.ValueString(),
			ConcurrencyPolicy: cron.ConcurrencyPolicy.ValueString(),
		}
	}

	if !plan.RuntimeEnvironment.IsNull() && !plan.RuntimeEnvironment.IsUnknown() {
		diags.Append(plan.RuntimeEnvironment.ElementsAs(ctx, &payload.RuntimeEnvironment, false)...)
	}

	var deployment jobDeploymentModel
	diags.Append(plan.Deployment.As(ctx, &deployment, objectAsOptions)...)

	if diags.HasError() {
		return nil, diags
	}

	payload.Deployment.Docker = dockerOverride(knownStringPointer(deployment.Command), knownStringPointer(deployment.Entrypoint))

	switch {
	case !deployment.ExternalImage.IsNull() && !deployment.ExternalImage.IsUnknown():
		var image jobExternalImageModel
		diags.Append(deployment.ExternalImage.As(ctx, &image, objectAsOptions)...)

		payload.Deployment.External = &jobExternalPayload{
			ImagePath:   image.ImagePath.ValueString(),
			Credentials: knownStringPointer(image.CredentialsID),
		}

	case !deployment.BuildService.IsNull() && !deployment.BuildService.IsUnknown():
		var build jobBuildServiceModel
		diags.Append(deployment.BuildService.As(ctx, &build, objectAsOptions)...)

		payload.Deployment.Internal = &jobInternalPayload{
			ID:       build.ID.ValueString(),
			Branch:   knownStringPointer(build.Branch),
			BuildSHA: knownStringPointer(build.BuildSHA),
		}

	default:
		// Guarded by the schema's ExactlyOneOf, so this is a provider bug.
		diags.AddError("Invalid job deployment", "Exactly one of external_image or build_service must be set.")
	}

	return payload, diags
}

// applyJob copies an API response onto the Terraform model. What the API does
// not report -- the deployment source and the runtime environment -- keeps
// the model's value.
func applyJob(ctx context.Context, model *jobResourceModel, job *jobView) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(job.ID)
	model.UID = types.StringValue(job.UID)
	model.AppID = types.StringValue(job.AppID)
	model.ProjectID = types.StringValue(job.ProjectID)
	model.Name = types.StringValue(job.Name)
	model.Description = types.StringPointerValue(job.Description)
	model.DeploymentPlan = types.StringValue(job.DeploymentPlan)
	model.BackoffLimit = types.Int64Value(int64(job.BackoffLimit))
	model.JobType = types.StringValue(job.JobType)
	model.Suspended = types.BoolValue(job.Suspended)
	model.CreatedAt = timeValue(job.CreatedAt)

	// Zero means "not set" to the API.
	if job.ActiveDeadlineSeconds > 0 {
		model.ActiveDeadlineSeconds = types.Int64Value(int64(job.ActiveDeadlineSeconds))
	} else {
		model.ActiveDeadlineSeconds = types.Int64Null()
	}

	tags, d := types.SetValueFrom(ctx, types.StringType, nonNil(job.Tags))
	diags.Append(d...)
	model.Tags = tags

	cron, d := jobCronValue(job.Cron)
	diags.Append(d...)
	model.Cron = cron

	deployment, d := jobDeploymentValue(ctx, model.Deployment, job)
	diags.Append(d...)
	model.Deployment = deployment

	if model.RuntimeEnvironment.IsUnknown() {
		model.RuntimeEnvironment = types.MapNull(types.StringType)
	}

	return diags
}

func jobCronValue(cron *jobCronPayload) (types.Object, diag.Diagnostics) {
	if cron == nil {
		return types.ObjectNull(jobCronAttrTypes), nil
	}

	return types.ObjectValue(jobCronAttrTypes, map[string]attr.Value{
		"schedule":           types.StringValue(cron.Schedule),
		"concurrency_policy": types.StringValue(cron.ConcurrencyPolicy),
	})
}

// jobDeploymentValue keeps the configured deployment source, which the API
// does not report, and takes the command and entrypoint overrides from the
// API, which it does.
func jobDeploymentValue(ctx context.Context, current types.Object, job *jobView) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics

	deployment := jobDeploymentModel{
		ExternalImage: types.ObjectNull(jobExternalImageAttrTypes),
		BuildService:  types.ObjectNull(jobBuildServiceAttrTypes),
	}

	if !current.IsNull() && !current.IsUnknown() {
		diags.Append(current.As(ctx, &deployment, objectAsOptions)...)
	}

	deployment.Command = types.StringPointerValue(job.Command)
	deployment.Entrypoint = types.StringPointerValue(job.Entrypoint)

	value, d := types.ObjectValueFrom(ctx, jobDeploymentAttrTypes, deployment)
	diags.Append(d...)

	return value, diags
}
