package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ resource.Resource                   = (*addonBackupScheduleResource)(nil)
	_ resource.ResourceWithConfigure      = (*addonBackupScheduleResource)(nil)
	_ resource.ResourceWithImportState    = (*addonBackupScheduleResource)(nil)
	_ resource.ResourceWithValidateConfig = (*addonBackupScheduleResource)(nil)
)

// Enumerations copied from the OpenAPI document.
var (
	backupTypes             = []string{"dump", "snapshot"}
	backupIntervals         = []string{"hourly", "daily", "weekly"}
	backupCompressionTypes  = []string{"gz", "zstd"}
	backupDestinationIDSpec = secretGroupIdentifierPattern
)

// addonBackupScheduleResource manages one backup schedule of an addon. The
// API can create, list and delete schedules but not change one, so every
// configurable attribute forces a new schedule.
type addonBackupScheduleResource struct {
	client *northflank.ClientWithResponses
}

// NewAddonBackupScheduleResource returns the northflank_addon_backup_schedule
// resource implementation.
func NewAddonBackupScheduleResource() resource.Resource {
	return &addonBackupScheduleResource{}
}

type addonBackupScheduleModel struct {
	ID                  types.String `tfsdk:"id"`
	ProjectID           types.String `tfsdk:"project_id"`
	AddonID             types.String `tfsdk:"addon_id"`
	BackupType          types.String `tfsdk:"backup_type"`
	Interval            types.String `tfsdk:"interval"`
	Minutes             types.Set    `tfsdk:"minutes"`
	Hours               types.Set    `tfsdk:"hours"`
	Days                types.Set    `tfsdk:"days"`
	RetentionDays       types.Int64  `tfsdk:"retention_days"`
	CompressionType     types.String `tfsdk:"compression_type"`
	CustomDestinationID types.String `tfsdk:"custom_destination_id"`
}

// backupSchedulePayload is the JSON body for creating a schedule, and the
// shape one is reported in, apart from the compression type's name.
type backupSchedulePayload struct {
	Scheduling          backupSchedulingPayload `json:"scheduling"`
	BackupType          string                  `json:"backupType"`
	RetentionTime       int                     `json:"retentionTime"`
	CompressionType     *string                 `json:"compressionType,omitempty"`
	CustomDestinationID *string                 `json:"customDestinationId,omitempty"`
}

type backupSchedulingPayload struct {
	Interval string `json:"interval"`
	Minute   []int  `json:"minute"`
	Hour     []int  `json:"hour,omitempty"`
	Day      []int  `json:"day,omitempty"`
}

// backupScheduleRead is one schedule as the list reports it.
type backupScheduleRead struct {
	ID                    string                  `json:"id"`
	BackupType            string                  `json:"backupType"`
	BackupCompressionType *string                 `json:"backupCompressionType"`
	CustomDestinationID   *string                 `json:"customDestinationId"`
	Scheduling            backupSchedulingPayload `json:"scheduling"`
	RetentionTime         int                     `json:"retentionTime"`
}

func (r *addonBackupScheduleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_addon_backup_schedule"
}

func (r *addonBackupScheduleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	replaceSet := []planmodifier.Set{setplanmodifier.RequiresReplace()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Schedules backups of an addon, such as a daily dump of a PostgreSQL database.\n\n" +
			"Northflank cannot change a schedule, so changing any attribute replaces it. An addon can have one " +
			"schedule per interval and backup type. Destroying the schedule stops new backups; existing backups " +
			"are kept until their retention time passes.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "ID of the schedule, assigned by Northflank.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the addon belongs to.",
				PlanModifiers:       replace,
			},
			"addon_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the addon to back up.",
				PlanModifiers:       replace,
			},
			"backup_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "`dump`, a logical export of the data, or `snapshot`, a copy of the addon's " +
					"storage. Which types an addon supports depends on its type.",
				Validators:    []validator.String{stringvalidator.OneOf(backupTypes...)},
				PlanModifiers: replace,
			},
			"interval": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "`hourly`, `daily` or `weekly`. `daily` needs `hours`; `weekly` needs `hours` " +
					"and `days`.",
				Validators:    []validator.String{stringvalidator.OneOf(backupIntervals...)},
				PlanModifiers: replace,
			},
			"minutes": schema.SetAttribute{
				Required:            true,
				ElementType:         types.Int64Type,
				MarkdownDescription: "Minutes past the hour at which to back up, from 0 to 59.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueInt64sAre(int64validator.Between(0, 59)),
				},
				PlanModifiers: replaceSet,
			},
			"hours": schema.SetAttribute{
				Optional:    true,
				ElementType: types.Int64Type,
				MarkdownDescription: "Hours of the day at which to back up, from 0 to 23 (24-hour clock). Required for " +
					"`daily` and `weekly`, not allowed for `hourly`.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueInt64sAre(int64validator.Between(0, 23)),
				},
				PlanModifiers: replaceSet,
			},
			"days": schema.SetAttribute{
				Optional:    true,
				ElementType: types.Int64Type,
				MarkdownDescription: "Days of the week on which to back up, from `0` (Monday) to `6` (Sunday). " +
					"Required for `weekly`, not allowed otherwise.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueInt64sAre(int64validator.Between(0, 6)),
				},
				PlanModifiers: replaceSet,
			},
			"retention_days": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "How many days each backup is kept.",
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"compression_type": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Compression of `dump` backups, `gz` or `zstd`. Northflank uses `gz` when it " +
					"is not set. Not allowed for `snapshot`.",
				Validators:    []validator.String{stringvalidator.OneOf(backupCompressionTypes...)},
				PlanModifiers: replace,
			},
			"custom_destination_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "ID of a custom backup destination, such as an S3 bucket added to the team, " +
					"to store the backups in instead of Northflank's own storage.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 100),
					stringvalidator.RegexMatches(backupDestinationIDSpec, "must be a backup destination ID"),
				},
				PlanModifiers: replace,
			},
		},
	}
}

// ValidateConfig enforces the combinations the API documents: which of hours
// and days each interval takes, and compression only for dumps.
func (r *addonBackupScheduleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config addonBackupScheduleModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(validateBackupSchedule(config)...)
}

func validateBackupSchedule(config addonBackupScheduleModel) diag.Diagnostics {
	var diags diag.Diagnostics

	// Values not yet known are checked at apply time by the API instead.
	if config.Interval.IsUnknown() || config.Hours.IsUnknown() || config.Days.IsUnknown() {
		return diags
	}

	interval := config.Interval.ValueString()
	wantHours := interval == "daily" || interval == "weekly"
	wantDays := interval == "weekly"

	check := func(attr string, set types.Set, want bool) {
		switch {
		case want && set.IsNull():
			diags.AddAttributeError(path.Root(attr), "Missing "+attr,
				fmt.Sprintf("A %s backup schedule needs %s.", interval, attr))
		case !want && !set.IsNull():
			diags.AddAttributeError(path.Root(attr), "Unexpected "+attr,
				fmt.Sprintf("A %s backup schedule does not take %s.", interval, attr))
		}
	}

	if !config.Interval.IsNull() {
		check("hours", config.Hours, wantHours)
		check("days", config.Days, wantDays)
	}

	if config.BackupType.ValueString() == "snapshot" && !config.CompressionType.IsNull() && !config.CompressionType.IsUnknown() {
		diags.AddAttributeError(path.Root("compression_type"), "Unexpected compression_type",
			"compression_type only applies to dump backups.")
	}

	return diags
}

func (r *addonBackupScheduleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *addonBackupScheduleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan addonBackupScheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	payload, diags := backupSchedulePayloadFrom(ctx, plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, err := json.Marshal(payload)

	if err != nil {
		resp.Diagnostics.AddError("Error encoding backup schedule", err.Error())
		return
	}

	projectID, addonID := plan.ProjectID.ValueString(), plan.AddonID.ValueString()

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostProjectsAddonsAddonidBackupSchedulesWithBodyWithResponse(ctx, projectID, addonID,
		"application/json", bytes.NewReader(body))

	if err == nil {
		err = northflank.CheckResponse(created)
	}

	if err != nil {
		resp.Diagnostics.AddError("Error creating Northflank backup schedule",
			fmt.Sprintf("Could not create a %s %s backup schedule for addon %q: %s",
				payload.Scheduling.Interval, payload.BackupType, addonID, err))
		return
	}

	read, err := decodeData[struct {
		ID string `json:"id"`
	}](created.Body, "backup schedule")

	if err != nil {
		resp.Diagnostics.AddError("Error reading created Northflank backup schedule", err.Error())
		return
	}

	tflog.Debug(ctx, "created Northflank backup schedule", map[string]any{"addon_id": addonID, "id": read.ID})

	// Everything else is as planned; the schedule cannot differ from what was
	// sent without being a different schedule.
	plan.ID = types.StringValue(read.ID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *addonBackupScheduleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state addonBackupScheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	schedule, err := getBackupSchedule(ctx, r.client, state.ProjectID.ValueString(), state.AddonID.ValueString(), state.ID.ValueString())

	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError("Error reading Northflank backup schedule", err.Error())
		return
	}

	// An imported schedule has only its IDs in state.
	imported := state.BackupType.IsNull()

	resp.Diagnostics.Append(applyBackupSchedule(ctx, &state, schedule, !imported)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never called with a change: every configurable attribute forces
// a replacement.
func (r *addonBackupScheduleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan addonBackupScheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *addonBackupScheduleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state addonBackupScheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, addonID, id := state.ProjectID.ValueString(), state.AddonID.ValueString(), state.ID.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteProjectsAddonsAddonidBackupSchedulesScheduleidWithResponse(ctx, projectID, addonID, id)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(deleted)
	})

	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Error deleting Northflank backup schedule",
			fmt.Sprintf("Could not delete backup schedule %q of addon %q: %s", id, addonID, err))
	}
}

// ImportState takes <project_id>/<addon_id>/<schedule_id>.
func (r *addonBackupScheduleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")

	if len(parts) != 3 || slices.Contains(parts, "") {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Expected <project_id>/<addon_id>/<schedule_id>, got %q.", req.ID))
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("addon_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[2])...)
}

// getBackupSchedule finds a schedule in the addon's list, or returns a 404
// when the addon no longer has it. An addon has at most one schedule per
// interval and backup type, so one page holds them all.
func getBackupSchedule(ctx context.Context, client *northflank.ClientWithResponses, projectID, addonID, id string) (*backupScheduleRead, error) {
	var found *backupScheduleRead
	perPage := 100

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsAddonsAddonidBackupSchedulesWithResponse(ctx, projectID, addonID,
			&northflank.GetProjectsAddonsAddonidBackupSchedulesParams{PerPage: &perPage})

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		list, err := decodeData[struct {
			Schedules []backupScheduleRead `json:"schedules"`
		}](got.Body, "backup schedules")

		if err != nil {
			return err
		}

		for i := range list.Schedules {
			if list.Schedules[i].ID == id {
				found = &list.Schedules[i]
				return nil
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if found == nil {
		return nil, &northflank.APIError{StatusCode: http.StatusNotFound, Message: fmt.Sprintf("addon %q has no backup schedule %q", addonID, id)}
	}

	return found, nil
}

func backupSchedulePayloadFrom(ctx context.Context, plan addonBackupScheduleModel) (backupSchedulePayload, diag.Diagnostics) {
	var diags diag.Diagnostics

	payload := backupSchedulePayload{
		Scheduling: backupSchedulingPayload{
			Interval: plan.Interval.ValueString(),
			Minute:   intsFromSet(ctx, plan.Minutes, &diags),
			Hour:     intsFromSet(ctx, plan.Hours, &diags),
			Day:      intsFromSet(ctx, plan.Days, &diags),
		},
		BackupType:          plan.BackupType.ValueString(),
		RetentionTime:       int(plan.RetentionDays.ValueInt64()),
		CompressionType:     plan.CompressionType.ValueStringPointer(),
		CustomDestinationID: plan.CustomDestinationID.ValueStringPointer(),
	}

	return payload, diags
}

// applyBackupSchedule sets the model from a reported schedule. With a prior
// model, the optional compression type and destination keep being unset when
// they were, since Northflank reports its default compression.
func applyBackupSchedule(ctx context.Context, model *addonBackupScheduleModel, read *backupScheduleRead, hasPrior bool) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(read.ID)
	model.BackupType = types.StringValue(read.BackupType)
	model.Interval = types.StringValue(read.Scheduling.Interval)
	model.Minutes = int64SetOrNull(ctx, read.Scheduling.Minute, &diags)
	model.Hours = int64SetOrNull(ctx, read.Scheduling.Hour, &diags)
	model.Days = int64SetOrNull(ctx, read.Scheduling.Day, &diags)
	model.RetentionDays = types.Int64Value(int64(read.RetentionTime))
	model.CompressionType = reportedOptional(model.CompressionType, read.BackupCompressionType, hasPrior)
	model.CustomDestinationID = reportedOptional(model.CustomDestinationID, read.CustomDestinationID, hasPrior)

	return diags
}

func intsFromSet(ctx context.Context, set types.Set, diags *diag.Diagnostics) []int {
	if set.IsNull() || set.IsUnknown() {
		return nil
	}

	var values []int64
	diags.Append(set.ElementsAs(ctx, &values, false)...)

	out := make([]int, 0, len(values))

	for _, v := range values {
		out = append(out, int(v))
	}

	slices.Sort(out)
	return out
}

// int64SetOrNull converts reported integers into a set, or null for none,
// matching attributes that are optional and have no default.
func int64SetOrNull(ctx context.Context, values []int, diags *diag.Diagnostics) types.Set {
	if len(values) == 0 {
		return types.SetNull(types.Int64Type)
	}

	set, d := types.SetValueFrom(ctx, types.Int64Type, values)
	diags.Append(d...)

	return set
}
