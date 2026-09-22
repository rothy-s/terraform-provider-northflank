package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
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
	_ resource.Resource                   = (*addonResource)(nil)
	_ resource.ResourceWithConfigure      = (*addonResource)(nil)
	_ resource.ResourceWithImportState    = (*addonResource)(nil)
	_ resource.ResourceWithValidateConfig = (*addonResource)(nil)
)

const addonLatestVersion = "latest"

type addonResource struct {
	client *northflank.ClientWithResponses
}

// NewAddonResource returns the northflank_addon resource implementation.
func NewAddonResource() resource.Resource {
	return &addonResource{}
}

// addonResourceModel maps the northflank_addon schema.
type addonResourceModel struct {
	ID                types.String `tfsdk:"id"`
	UID               types.String `tfsdk:"uid"`
	AppID             types.String `tfsdk:"app_id"`
	ProjectID         types.String `tfsdk:"project_id"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	Tags              types.Set    `tfsdk:"tags"`
	Type              types.String `tfsdk:"type"`
	Version           types.String `tfsdk:"version"`
	DeploymentPlan    types.String `tfsdk:"deployment_plan"`
	Replicas          types.Int64  `tfsdk:"replicas"`
	StorageSize       types.Int64  `tfsdk:"storage_size"`
	StorageClass      types.String `tfsdk:"storage_class"`
	DatabaseName      types.String `tfsdk:"database_name"`
	TLSEnabled        types.Bool   `tfsdk:"tls_enabled"`
	ExternalAccess    types.Bool   `tfsdk:"external_access"`
	VpcAccessible     types.Bool   `tfsdk:"vpc_accessible"`
	Status            types.String `tfsdk:"status"`
	RunningVersion    types.String `tfsdk:"running_version"`
	Region            types.String `tfsdk:"region"`
	Secrets           types.Map    `tfsdk:"secrets"`
	ConnectionDetails types.Map    `tfsdk:"connection_details"`
	CreatedAt         types.String `tfsdk:"created_at"`
}

func (r *addonResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_addon"
}

func (r *addonResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Northflank addon: a managed database, cache or message queue such as " +
			"PostgreSQL, MySQL, MongoDB, Redis or RabbitMQ.\n\n" +
			"Creating an addon waits until it is running, so that `secrets` and `connection_details` are " +
			"usable by whatever depends on it. Destroying it waits until it is gone.\n\n" +
			"**Destroying this resource deletes the addon and its data**, and so does changing `name`, " +
			"`type`, `project_id` or `database_name`. Consider `lifecycle { prevent_destroy = true }`. " +
			"Credentials are stored in Terraform state; treat the state file as a secret.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identifier of the addon, derived by Northflank from `name`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"uid": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Permanent identifier of the addon. Unlike `id`, it never changes and is never reused.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"app_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Full identifier Northflank uses for the addon.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the addon belongs to. Changing this forces a new addon.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the addon, 3 to 39 characters. Changing this forces a new addon, " +
					"since Northflank derives the `id` from it and cannot rename one.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 39),
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
				MarkdownDescription: "Short description of the addon, up to 200 characters. Removing it from " +
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
				MarkdownDescription: "Previously defined Northflank tags to attach to the addon.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(secretGroupIdentifierValidators()...),
				},
			},
			"type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Kind of addon, such as `postgresql`, `mysql`, `mongodb`, `redis`, `rabbitmq` " +
					"or `minio`. The Northflank API lists the types available to a team at `GET /v1/addon-types`. " +
					"Changing this forces a new addon.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"version": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Version to run, such as `16.4.0`, a prefix of one such as `16`, or `latest` " +
					"for the newest available at creation. Raising it upgrades the addon in place; Northflank may " +
					"refuse to downgrade. `running_version` shows the exact version, and a newer release is not " +
					"applied until `version` asks for it.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"deployment_plan": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the compute plan each replica uses, such as `nf-compute-20`.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 100),
					stringvalidator.RegexMatches(jobPlanPattern, "must be a Northflank plan ID such as nf-compute-20"),
				},
			},
			"replicas": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(1),
				MarkdownDescription: "Number of replicas to run. Defaults to `1`.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"storage_size": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Storage per replica, in megabytes, such as `4096`. Changes are applied in " +
					"place; Northflank may refuse to shrink storage.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"storage_class": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Storage class, such as `nvme`. Only configurable on accounts with the " +
					"feature enabled; Northflank picks its default otherwise.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"database_name": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Name of the database to create, for types that support choosing it. Can " +
					"only be set at creation, so changing it forces a new addon. The API does not report it back.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"tls_enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Serve connections over TLS, for types that support it. Defaults to `false`.",
			},
			"external_access": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Make the addon reachable from the internet, over TLS. Cannot be combined " +
					"with `vpc_accessible`. Defaults to `false`.",
			},
			"vpc_accessible": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Expose the addon on the cluster's private (VPC) load balancer. Cannot be " +
					"combined with `external_access`. Defaults to `false`.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "State of the addon, such as `running`, `paused` or `upgrading`.",
			},
			"running_version": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Version the addon actually runs.",
			},
			"region": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Region the addon is deployed in.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"secrets": schema.MapAttribute{
				Computed:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "Secrets for connecting, such as `username` and `password`. Which keys exist " +
					"depends on the addon type.",
			},
			"connection_details": schema.MapAttribute{
				Computed:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "Connection details such as hosts, ports and connection strings, keyed as " +
					"Northflank names them for the addon type.",
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

func (r *addonResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var external, vpc types.Bool
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("external_access"), &external)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("vpc_accessible"), &vpc)...)

	if external.ValueBool() && vpc.ValueBool() {
		resp.Diagnostics.AddAttributeError(
			path.Root("vpc_accessible"),
			"Conflicting addon networking",
			"external_access and vpc_accessible cannot both be true.",
		)
	}
}

func (r *addonResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *addonResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan addonResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	payload := addonCreatePayload{
		Name:        plan.Name.ValueString(),
		Description: knownStringPointer(plan.Description),
		Tags:        stringsFromSet(ctx, plan.Tags, &resp.Diagnostics),
		Type:        plan.Type.ValueString(),
		Version:     plan.Version.ValueString(),
		Billing: addonBillingPayload{
			DeploymentPlan: plan.DeploymentPlan.ValueString(),
			Replicas:       int(plan.Replicas.ValueInt64()),
			Storage:        int(plan.StorageSize.ValueInt64()),
			StorageClass:   knownStringPointer(plan.StorageClass),
		},
		TLSEnabled:            plan.TLSEnabled.ValueBool(),
		ExternalAccessEnabled: plan.ExternalAccess.ValueBool(),
		VpcAccessible:         plan.VpcAccessible.ValueBool(),
	}

	if name := knownStringPointer(plan.DatabaseName); name != nil {
		payload.CustomCredentials = &addonCredentialPayload{DBName: *name}
	}

	body, err := json.Marshal(payload)

	if err != nil {
		resp.Diagnostics.AddError("Error building Northflank addon request", err.Error())
		return
	}

	projectID := plan.ProjectID.ValueString()

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostProjectsAddonsWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))

	if err == nil {
		err = northflank.CheckResponse(created)
	}

	var addon *addonRead

	if err == nil {
		addon, err = addonFromBody(created.Body)
	}

	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank addon",
			fmt.Sprintf("Could not create addon %q in project %q: %s", plan.Name.ValueString(), projectID, err),
		)
		return
	}

	addonID := addon.ID
	tflog.Debug(ctx, "created Northflank addon, waiting for it to run", map[string]any{"project_id": projectID, "id": addonID})

	// The addon exists from here on, so a failed wait must still leave state
	// behind or Terraform leaks it. Seed every computed attribute first.
	plan.ID = types.StringValue(addonID)
	plan.UID = types.StringValue(addon.UID)
	plan.AppID = types.StringValue(addon.AppID)
	plan.Status = types.StringValue(addon.Status)
	plan.RunningVersion = types.StringNull()
	plan.Region = types.StringNull()
	plan.Secrets = types.MapNull(types.StringType)
	plan.ConnectionDetails = types.MapNull(types.StringType)
	plan.CreatedAt = timeValue(addon.CreatedAt)

	if plan.Description.IsUnknown() {
		plan.Description = types.StringNull()
	}

	if plan.StorageClass.IsUnknown() {
		plan.StorageClass = types.StringNull()
	}

	addon, err = waitForAddon(ctx, r.client, projectID, addonID)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Northflank addon did not become ready",
			fmt.Sprintf("The addon was created with ID %q but did not reach a running state: %s. It has been "+
				"saved to state; a later apply picks it up once it is running.", addonID, err),
		)
		return
	}

	resp.Diagnostics.Append(r.refresh(ctx, &plan, addon)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *addonResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state addonResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, addonID := state.ProjectID.ValueString(), state.ID.ValueString()
	addon, err := getAddon(ctx, r.client, projectID, addonID)

	if err != nil {
		if isNotFound(err) {
			tflog.Debug(ctx, "Northflank addon no longer exists, removing from state",
				map[string]any{"project_id": projectID, "id": addonID})
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Error reading Northflank addon",
			fmt.Sprintf("Could not read addon %q in project %q: %s", addonID, projectID, err),
		)
		return
	}

	resp.Diagnostics.Append(r.refresh(ctx, &state, addon)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *addonResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state addonResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, addonID := state.ProjectID.ValueString(), state.ID.ValueString()
	patch := addonPatchFrom(ctx, &plan, &state, &resp.Diagnostics)

	if resp.Diagnostics.HasError() {
		return
	}

	if !patch.empty() {
		body, _ := json.Marshal(patch)

		// Not retried: a version upgrade or resize that timed out may still
		// be under way, and repeating it is not obviously safe.
		updated, err := r.client.PatchProjectsAddonsAddonidWithBodyWithResponse(
			ctx, projectID, addonID, "application/json", bytes.NewReader(body))

		if err == nil {
			err = northflank.CheckResponse(updated)
		}

		if err != nil {
			resp.Diagnostics.AddError(
				"Error updating Northflank addon",
				fmt.Sprintf("Could not update addon %q in project %q: %s", addonID, projectID, err),
			)
			return
		}
	}

	// Upgrades and resizes run asynchronously; wait for them to finish.
	addon, err := waitForAddon(ctx, r.client, projectID, addonID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Northflank addon did not become ready",
			fmt.Sprintf("Addon %q was updated but did not return to a running state: %s", addonID, err),
		)
		return
	}

	resp.Diagnostics.Append(r.refresh(ctx, &plan, addon)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *addonResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state addonResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, addonID := state.ProjectID.ValueString(), state.ID.ValueString()

	deleted, err := r.client.DeleteProjectsAddonsAddonidWithResponse(ctx, projectID, addonID)

	if err == nil {
		err = northflank.CheckResponse(deleted)
	}

	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError(
			"Error deleting Northflank addon",
			fmt.Sprintf("Could not delete addon %q in project %q: %s", addonID, projectID, err),
		)
		return
	}

	if err := waitForAddonDeleted(ctx, r.client, projectID, addonID); err != nil {
		resp.Diagnostics.AddError(
			"Error deleting Northflank addon",
			fmt.Sprintf("Addon %q was asked to delete but is still there: %s", addonID, err),
		)
	}
}

func (r *addonResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, addonID, ok := strings.Cut(req.ID, "/")

	if !ok || projectID == "" || addonID == "" || strings.Contains(addonID, "/") {
		resp.Diagnostics.AddError(
			"Invalid import identifier",
			fmt.Sprintf("Expected <project_id>/<addon_id>, got %q. Import with: "+
				"terraform import northflank_addon.example my-project/my-addon", req.ID),
		)
		return
	}

	// Read fills in everything the API reports; database_name comes from
	// configuration.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), addonID)...)
}

// refresh applies an addon and its credentials to the model. Credentials are
// only available while the addon runs; when they cannot be fetched, the
// previous values stand rather than failing the refresh.
func (r *addonResource) refresh(ctx context.Context, model *addonResourceModel, addon *addonRead) diag.Diagnostics {
	diags := applyAddon(ctx, model, addon)

	creds, err := getAddonCredentials(ctx, r.client, model.ProjectID.ValueString(), addon.ID)

	if err != nil {
		tflog.Warn(ctx, "could not read Northflank addon credentials", map[string]any{"id": addon.ID, "error": err.Error()})

		if model.Secrets.IsUnknown() {
			model.Secrets = types.MapNull(types.StringType)
		}

		if model.ConnectionDetails.IsUnknown() {
			model.ConnectionDetails = types.MapNull(types.StringType)
		}

		return diags
	}

	secrets, envs, d := credentialMaps(ctx, creds)
	diags.Append(d...)
	model.Secrets = secrets
	model.ConnectionDetails = envs

	return diags
}

// applyAddon copies an API response onto the Terraform model.
func applyAddon(ctx context.Context, model *addonResourceModel, addon *addonRead) diag.Diagnostics {
	var diags diag.Diagnostics
	config := addon.Spec.Config

	model.ID = types.StringValue(addon.ID)
	model.UID = types.StringValue(addon.UID)
	model.AppID = types.StringValue(addon.AppID)
	model.Name = types.StringValue(addon.Name)
	model.Description = types.StringPointerValue(addon.Description)
	model.Type = types.StringValue(addon.Spec.Type)
	model.Status = types.StringValue(addon.Status)
	model.RunningVersion = types.StringValue(config.VersionTag)
	model.CreatedAt = timeValue(addon.CreatedAt)

	// The configured version stays as written while the addon runs a version
	// it describes; otherwise what runs is recorded, so that an upgrade made
	// outside Terraform shows as drift.
	if !addonVersionMatches(model.Version.ValueString(), config.VersionTag) {
		model.Version = types.StringValue(config.VersionTag)
	}

	if d := config.Deployment; d != nil {
		model.DeploymentPlan = types.StringValue(d.PlanID)
		model.Replicas = types.Int64Value(int64(d.Replicas))
		model.StorageSize = types.Int64Value(int64(d.StorageSize))
		model.StorageClass = types.StringValue(d.StorageClass)
		model.Region = types.StringValue(d.Region)
	}

	if n := config.Networking; n != nil {
		model.TLSEnabled = types.BoolValue(n.TLSEnabled)
		model.ExternalAccess = types.BoolValue(n.ExternalAccessEnabled)
		model.VpcAccessible = types.BoolValue(n.VpcAccessible)
	}

	tags, d := types.SetValueFrom(ctx, types.StringType, nonNil(addon.Tags))
	diags.Append(d...)
	model.Tags = tags

	return diags
}

// addonPatchFrom builds a patch with only the attributes that changed.
func addonPatchFrom(ctx context.Context, plan, state *addonResourceModel, diags *diag.Diagnostics) *addonPatchPayload {
	patch := &addonPatchPayload{}

	if v := knownStringPointer(plan.Description); v != nil && !plan.Description.Equal(state.Description) {
		patch.Description = v
	}

	if !plan.Tags.Equal(state.Tags) {
		tags := stringsFromSet(ctx, plan.Tags, diags)
		patch.Tags = &tags
	}

	if !plan.Version.Equal(state.Version) {
		patch.Version = plan.Version.ValueStringPointer()
	}

	billing := &addonBillingPatchPayload{}

	if !plan.DeploymentPlan.Equal(state.DeploymentPlan) {
		billing.DeploymentPlan = plan.DeploymentPlan.ValueStringPointer()
	}

	if !plan.Replicas.Equal(state.Replicas) {
		billing.Replicas = knownIntPointer(plan.Replicas)
	}

	if !plan.StorageSize.Equal(state.StorageSize) {
		billing.Storage = knownIntPointer(plan.StorageSize)
	}

	if v := knownStringPointer(plan.StorageClass); v != nil && !plan.StorageClass.Equal(state.StorageClass) {
		billing.StorageClass = v
	}

	if *billing != (addonBillingPatchPayload{}) {
		patch.Billing = billing
	}

	if !plan.TLSEnabled.Equal(state.TLSEnabled) {
		patch.TLSEnabled = plan.TLSEnabled.ValueBoolPointer()
	}

	if !plan.ExternalAccess.Equal(state.ExternalAccess) {
		patch.ExternalAccessEnabled = plan.ExternalAccess.ValueBoolPointer()
	}

	if !plan.VpcAccessible.Equal(state.VpcAccessible) {
		patch.VpcAccessible = plan.VpcAccessible.ValueBoolPointer()
	}

	return patch
}

// addonVersionMatches reports whether the running version is one the
// configured version describes: the same, a release under a configured
// prefix ("16" runs "16.4.0"), or anything for "latest". An unreported
// running version, as while an addon starts, counts as a match; no configured
// version, as after an import, does not, so the running one is recorded.
func addonVersionMatches(configured, running string) bool {
	switch {
	case configured == "":
		return false
	case running == "", configured == addonLatestVersion, configured == running:
		return true
	default:
		return strings.HasPrefix(running, configured+".")
	}
}
