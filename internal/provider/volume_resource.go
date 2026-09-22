package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
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
	_ resource.Resource                = (*volumeResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeResource)(nil)
	_ resource.ResourceWithImportState = (*volumeResource)(nil)
)

// Enumerations and constraints copied from the OpenAPI document.
var (
	volumeAccessModes     = []string{"ReadWriteOnce", "ReadWriteMany"}
	volumeAttachmentTypes = []string{"service", "job"}

	// Container paths may not contain ':', volume sub-paths may not contain
	// '..'. RE2 has no lookahead, so both are checked with plain matches.
	volumeMountColon  = regexp.MustCompile(`:`)
	volumeMountParent = regexp.MustCompile(`\.\.`)
)

const volumeDefaultAccessMode = "ReadWriteOnce"

type volumeResource struct {
	client *northflank.ClientWithResponses
}

// NewVolumeResource returns the northflank_volume resource implementation.
func NewVolumeResource() resource.Resource {
	return &volumeResource{}
}

// volumeResourceModel maps the northflank_volume schema.
type volumeResourceModel struct {
	ID           types.String `tfsdk:"id"`
	UID          types.String `tfsdk:"uid"`
	ProjectID    types.String `tfsdk:"project_id"`
	Name         types.String `tfsdk:"name"`
	StorageSize  types.Int64  `tfsdk:"storage_size"`
	StorageClass types.String `tfsdk:"storage_class"`
	AccessMode   types.String `tfsdk:"access_mode"`
	Mounts       types.List   `tfsdk:"mounts"`
	Attachments  types.Set    `tfsdk:"attachments"`
	Status       types.String `tfsdk:"status"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

type volumeMountModel struct {
	ContainerPath types.String `tfsdk:"container_path"`
	VolumePath    types.String `tfsdk:"volume_path"`
}

type volumeAttachmentModel struct {
	ID   types.String `tfsdk:"id"`
	Type types.String `tfsdk:"type"`
}

var (
	volumeMountAttrTypes = map[string]attr.Type{
		"container_path": types.StringType,
		"volume_path":    types.StringType,
	}

	volumeAttachmentAttrTypes = map[string]attr.Type{
		"id":   types.StringType,
		"type": types.StringType,
	}
)

// ---- API payloads ----------------------------------------------------------

type volumeCreatePayload struct {
	Name            string                    `json:"name"`
	Spec            volumeSpecPayload         `json:"spec"`
	Mounts          []volumeMountPayload      `json:"mounts"`
	AttachedObjects []volumeAttachmentPayload `json:"attachedObjects"`
}

type volumeSpecPayload struct {
	AccessMode       string  `json:"accessMode"`
	StorageClassName *string `json:"storageClassName,omitempty"`
	StorageSize      int     `json:"storageSize"`
}

// volumeUpdatePayload carries the only two things the update endpoint
// changes: the mounts and the storage size.
type volumeUpdatePayload struct {
	Spec   volumeSizePayload    `json:"spec"`
	Mounts []volumeMountPayload `json:"mounts"`
}

type volumeSizePayload struct {
	StorageSize int `json:"storageSize"`
}

type volumeMountPayload struct {
	ContainerMountPath string  `json:"containerMountPath"`
	VolumeMountPath    *string `json:"volumeMountPath,omitempty"`
}

type volumeAttachmentPayload struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// volumeAttachRequest is the body of both attach and detach.
type volumeAttachRequest struct {
	NfObject volumeAttachmentPayload `json:"nfObject"`
}

// volumeView is the part of a volume the API reports. Mounts are not among
// it.
type volumeView struct {
	ID           string
	UID          *string
	Name         string
	StorageSize  int
	StorageClass *string
	AccessMode   string
	Attachments  []volumeAttachmentPayload
	Status       string
	CreatedAt    time.Time
}

// ---- Schema ----------------------------------------------------------------

func (r *volumeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

func (r *volumeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a persistent volume in a Northflank project, and which services or jobs " +
			"it is attached to.\n\n" +
			"**Destroying this resource deletes the volume and all data on it.** Consider " +
			"`lifecycle { prevent_destroy = true }`. Changing `name`, `project_id`, `access_mode` or " +
			"`storage_class` replaces the volume, which destroys its data too.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identifier of the volume, derived by Northflank from `name`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"uid": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Permanent identifier of the volume. Unlike `id`, it never changes and is never reused.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the volume belongs to. Changing this forces a new volume.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the volume, 3 to 39 characters. Changing this forces a new volume, " +
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
			"storage_size": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Size of the volume, in megabytes, such as `10240` for 10 GB. Which sizes are " +
					"available depends on the storage class. Changes are applied in place; Northflank may refuse " +
					"to shrink a volume.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"storage_class": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Storage class, such as `nvme`. Northflank picks its default when this is " +
					"not set. Changing this forces a new volume.",
				// UseStateForUnknown must run first, or leaving the attribute
				// out would plan as a change and force a replacement.
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"access_mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(volumeDefaultAccessMode),
				MarkdownDescription: "`ReadWriteOnce`, mountable by one instance at a time, or `ReadWriteMany`. " +
					"Only `ReadWriteOnce` is generally available. Defaults to `ReadWriteOnce`. Changing this forces " +
					"a new volume.",
				Validators: []validator.String{
					stringvalidator.OneOf(volumeAccessModes...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"mounts": schema.ListNestedAttribute{
				Required: true,
				MarkdownDescription: "Where the volume is mounted in the containers it is attached to. The API " +
					"does not report mounts back, so a change made outside Terraform is not detected, and an " +
					"imported volume shows a difference until the next apply.",
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
				},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"container_path": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "Path in the container to mount at, such as `/data`. Must be unique.",
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
								noMatchValidator{pattern: volumeMountColon, message: "must not contain ':'"},
							},
						},
						"volume_path": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "Directory inside the volume to mount, instead of its root.",
							Validators: []validator.String{
								noMatchValidator{pattern: volumeMountParent, message: "must not contain '..'"},
							},
						},
					},
				},
			},
			"attachments": schema.SetNestedAttribute{
				Optional: true,
				Computed: true,
				Default: setdefault.StaticValue(types.SetValueMust(
					types.ObjectType{AttrTypes: volumeAttachmentAttrTypes}, nil)),
				MarkdownDescription: "Services and jobs the volume is attached to. Attaching a volume to a " +
					"service restarts it. With `ReadWriteOnce`, attach it to one workload with one instance.",
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
							MarkdownDescription: "`service` or `job`.",
							Validators: []validator.String{
								stringvalidator.OneOf(volumeAttachmentTypes...),
							},
						},
					},
				},
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Status of the volume on the cluster, such as `BOUND`.",
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

func (r *volumeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

// ---- CRUD ------------------------------------------------------------------

func (r *volumeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	mounts, diags := volumeMountsPayload(ctx, plan.Mounts)
	resp.Diagnostics.Append(diags...)

	attachments, diags := volumeAttachmentsPayload(ctx, plan.Attachments)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, err := json.Marshal(volumeCreatePayload{
		Name: plan.Name.ValueString(),
		Spec: volumeSpecPayload{
			AccessMode:       plan.AccessMode.ValueString(),
			StorageClassName: knownStringPointer(plan.StorageClass),
			StorageSize:      int(plan.StorageSize.ValueInt64()),
		},
		Mounts:          mounts,
		AttachedObjects: attachments,
	})

	if err != nil {
		resp.Diagnostics.AddError("Error building Northflank volume request", err.Error())
		return
	}

	projectID := plan.ProjectID.ValueString()

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostProjectsVolumesWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))

	if err == nil {
		err = northflank.CheckResponse(created)
	}

	if err == nil && created.JSON200 == nil {
		err = errors.New("the API returned a successful status with no volume body")
	}

	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank volume",
			fmt.Sprintf("Could not create volume %q in project %q: %s", plan.Name.ValueString(), projectID, err),
		)
		return
	}

	data := created.JSON200.Data
	tflog.Debug(ctx, "created Northflank volume", map[string]any{"project_id": projectID, "id": data.Id})

	// The volume exists from here on, so a failed read-back must still leave
	// state behind or Terraform leaks it -- and a leaked volume costs money.
	plan.ID = types.StringValue(data.Id)
	plan.UID = types.StringPointerValue(data.Uid)
	plan.Status = types.StringValue(data.Status)
	plan.CreatedAt = timeValue(data.CreatedAt)

	if plan.StorageClass.IsUnknown() {
		plan.StorageClass = types.StringNull()
	}

	volume, err := getVolume(ctx, r.client, projectID, data.Id)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back created Northflank volume",
			fmt.Sprintf("The volume was created with ID %q but could not be read: %s. "+
				"It has been saved to state; the remaining attributes are populated on the next refresh.",
				data.Id, err),
		)
		return
	}

	resp.Diagnostics.Append(applyVolume(ctx, &plan, volume)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, volumeID := state.ProjectID.ValueString(), state.ID.ValueString()
	volume, err := getVolume(ctx, r.client, projectID, volumeID)

	if err != nil {
		if isNotFound(err) {
			tflog.Debug(ctx, "Northflank volume no longer exists, removing from state",
				map[string]any{"project_id": projectID, "id": volumeID})
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError(
			"Error reading Northflank volume",
			fmt.Sprintf("Could not read volume %q in project %q: %s", volumeID, projectID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyVolume(ctx, &state, volume)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *volumeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state volumeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, volumeID := state.ProjectID.ValueString(), state.ID.ValueString()

	// Size and mounts go through the update endpoint. It is sent whole, and
	// only when one of them changed, since resizing may be slow.
	if !plan.StorageSize.Equal(state.StorageSize) || !plan.Mounts.Equal(state.Mounts) {
		mounts, diags := volumeMountsPayload(ctx, plan.Mounts)
		resp.Diagnostics.Append(diags...)

		if resp.Diagnostics.HasError() {
			return
		}

		body, _ := json.Marshal(volumeUpdatePayload{
			Spec:   volumeSizePayload{StorageSize: int(plan.StorageSize.ValueInt64())},
			Mounts: mounts,
		})

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			updated, err := r.client.PostProjectsVolumesVolumeidWithBodyWithResponse(
				ctx, projectID, volumeID, "application/json", bytes.NewReader(body))

			if err != nil {
				return err
			}

			return northflank.CheckResponse(updated)
		})

		if err != nil {
			resp.Diagnostics.AddError(
				"Error updating Northflank volume",
				fmt.Sprintf("Could not update volume %q in project %q: %s", volumeID, projectID, err),
			)
			return
		}
	}

	// Attachments go through attach and detach, one object at a time.
	// Detaching first frees a ReadWriteOnce volume for its new workload.
	wanted, diags := volumeAttachmentsPayload(ctx, plan.Attachments)
	resp.Diagnostics.Append(diags...)

	current, diags := volumeAttachmentsPayload(ctx, state.Attachments)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	detach, attach := attachmentChanges(current, wanted)

	for _, object := range detach {
		if err := r.setAttachment(ctx, projectID, volumeID, object, false); err != nil {
			resp.Diagnostics.AddError(
				"Error detaching Northflank volume",
				fmt.Sprintf("Could not detach volume %q from %s %q: %s", volumeID, object.Type, object.ID, err),
			)
			return
		}
	}

	for _, object := range attach {
		if err := r.setAttachment(ctx, projectID, volumeID, object, true); err != nil {
			resp.Diagnostics.AddError(
				"Error attaching Northflank volume",
				fmt.Sprintf("Could not attach volume %q to %s %q: %s", volumeID, object.Type, object.ID, err),
			)
			return
		}
	}

	volume, err := getVolume(ctx, r.client, projectID, volumeID)

	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading Northflank volume",
			fmt.Sprintf("Volume %q was updated but could not be read back: %s", volumeID, err),
		)
		return
	}

	resp.Diagnostics.Append(applyVolume(ctx, &plan, volume)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, volumeID := state.ProjectID.ValueString(), state.ID.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteProjectsVolumesVolumeidWithResponse(ctx, projectID, volumeID)

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
		"Error deleting Northflank volume",
		fmt.Sprintf("Could not delete volume %q in project %q: %s", volumeID, projectID, err),
	)
}

func (r *volumeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, volumeID, ok := strings.Cut(req.ID, "/")

	if !ok || projectID == "" || volumeID == "" || strings.Contains(volumeID, "/") {
		resp.Diagnostics.AddError(
			"Invalid import identifier",
			fmt.Sprintf("Expected <project_id>/<volume_id>, got %q. Import with: "+
				"terraform import northflank_volume.example my-project/my-volume", req.ID),
		)
		return
	}

	// Read fills in everything the API reports; mounts come from the next
	// apply.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), volumeID)...)
}

// setAttachment attaches or detaches one object. Both are idempotent in
// effect, so they are retried.
func (r *volumeResource) setAttachment(ctx context.Context, projectID, volumeID string, object volumeAttachmentPayload, attach bool) error {
	body, _ := json.Marshal(volumeAttachRequest{NfObject: object})

	return apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		var (
			resp apierr.Response
			err  error
		)

		if attach {
			resp, err = r.client.PostProjectsVolumesVolumeidAttachWithBodyWithResponse(
				ctx, projectID, volumeID, "application/json", bytes.NewReader(body))
		} else {
			resp, err = r.client.PostProjectsVolumesVolumeidDetachWithBodyWithResponse(
				ctx, projectID, volumeID, "application/json", bytes.NewReader(body))
		}

		if err != nil {
			return err
		}

		return northflank.CheckResponse(resp)
	})
}

// ---- Helpers ---------------------------------------------------------------

// getVolume fetches a volume, retrying transient failures.
func getVolume(ctx context.Context, client *northflank.ClientWithResponses, projectID, volumeID string) (*volumeView, error) {
	var view *volumeView

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsVolumesVolumeidWithResponse(ctx, projectID, volumeID)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		if got.JSON200 == nil {
			return errors.New("the API returned a successful status with no volume body")
		}

		data := got.JSON200.Data
		view = &volumeView{
			ID:           data.Id,
			UID:          data.Uid,
			Name:         data.Name,
			StorageSize:  data.Spec.StorageSize,
			StorageClass: data.Spec.StorageClassName,
			AccessMode:   string(data.Spec.AccessMode),
			Status:       data.Status,
			CreatedAt:    data.CreatedAt,
		}

		if data.AttachedObjects != nil {
			for _, o := range *data.AttachedObjects {
				view.Attachments = append(view.Attachments, volumeAttachmentPayload{ID: o.Id, Type: string(o.Type)})
			}
		}

		return nil
	})

	return view, err
}

// applyVolume copies an API response onto the Terraform model. Mounts are not
// reported, so they keep the model's value.
func applyVolume(ctx context.Context, model *volumeResourceModel, volume *volumeView) diag.Diagnostics {
	var diags diag.Diagnostics

	model.ID = types.StringValue(volume.ID)
	model.UID = types.StringPointerValue(volume.UID)
	model.Name = types.StringValue(volume.Name)
	model.StorageSize = types.Int64Value(int64(volume.StorageSize))
	model.StorageClass = types.StringPointerValue(volume.StorageClass)
	model.AccessMode = types.StringValue(volume.AccessMode)
	model.Status = types.StringValue(volume.Status)
	model.CreatedAt = timeValue(volume.CreatedAt)

	attachments, d := volumeAttachmentsValue(ctx, volume.Attachments)
	diags.Append(d...)
	model.Attachments = attachments

	if model.Mounts.IsUnknown() {
		model.Mounts = types.ListNull(types.ObjectType{AttrTypes: volumeMountAttrTypes})
	}

	return diags
}

func volumeMountsPayload(ctx context.Context, value types.List) ([]volumeMountPayload, diag.Diagnostics) {
	out := []volumeMountPayload{}

	if value.IsNull() || value.IsUnknown() {
		return out, nil
	}

	var mounts []volumeMountModel
	diags := value.ElementsAs(ctx, &mounts, false)

	for _, m := range mounts {
		out = append(out, volumeMountPayload{
			ContainerMountPath: m.ContainerPath.ValueString(),
			VolumeMountPath:    knownStringPointer(m.VolumePath),
		})
	}

	return out, diags
}

// volumeAttachmentsPayload returns the attachments in a stable order.
func volumeAttachmentsPayload(ctx context.Context, value types.Set) ([]volumeAttachmentPayload, diag.Diagnostics) {
	out := []volumeAttachmentPayload{}

	if value.IsNull() || value.IsUnknown() {
		return out, nil
	}

	var models []volumeAttachmentModel
	diags := value.ElementsAs(ctx, &models, false)

	for _, m := range models {
		out = append(out, volumeAttachmentPayload{ID: m.ID.ValueString(), Type: m.Type.ValueString()})
	}

	sortAttachments(out)
	return out, diags
}

func volumeAttachmentsValue(ctx context.Context, attachments []volumeAttachmentPayload) (types.Set, diag.Diagnostics) {
	models := make([]volumeAttachmentModel, 0, len(attachments))

	for _, a := range attachments {
		models = append(models, volumeAttachmentModel{ID: types.StringValue(a.ID), Type: types.StringValue(a.Type)})
	}

	return types.SetValueFrom(ctx, types.ObjectType{AttrTypes: volumeAttachmentAttrTypes}, models)
}

// attachmentChanges works out which objects to detach and which to attach to
// get from current to wanted.
func attachmentChanges(current, wanted []volumeAttachmentPayload) (detach, attach []volumeAttachmentPayload) {
	in := func(list []volumeAttachmentPayload, a volumeAttachmentPayload) bool {
		for _, b := range list {
			if a == b {
				return true
			}
		}

		return false
	}

	for _, a := range current {
		if !in(wanted, a) {
			detach = append(detach, a)
		}
	}

	for _, a := range wanted {
		if !in(current, a) {
			attach = append(attach, a)
		}
	}

	return detach, attach
}

func sortAttachments(list []volumeAttachmentPayload) {
	sort.Slice(list, func(i, j int) bool {
		return list[i].Type+"/"+list[i].ID < list[j].Type+"/"+list[j].ID
	})
}

// noMatchValidator rejects strings that match a pattern, standing in for the
// negative lookaheads in the spec's patterns, which RE2 does not support.
type noMatchValidator struct {
	pattern *regexp.Regexp
	message string
}

func (v noMatchValidator) Description(context.Context) string { return v.message }

func (v noMatchValidator) MarkdownDescription(ctx context.Context) string { return v.message }

func (v noMatchValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	if v.pattern.MatchString(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid value", fmt.Sprintf("%q %s.", req.ConfigValue.ValueString(), v.message))
	}
}
