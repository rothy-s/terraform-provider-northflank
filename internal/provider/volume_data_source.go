package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ datasource.DataSource                     = (*volumeDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*volumeDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*volumeDataSource)(nil)
)

type volumeDataSource struct {
	client *northflank.ClientWithResponses
}

// NewVolumeDataSource returns the northflank_volume data source
// implementation.
func NewVolumeDataSource() datasource.DataSource {
	return &volumeDataSource{}
}

// volumeDataSourceModel maps the northflank_volume data source schema. Mounts
// are absent: the API does not report them.
type volumeDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	ProjectID    types.String `tfsdk:"project_id"`
	Name         types.String `tfsdk:"name"`
	UID          types.String `tfsdk:"uid"`
	StorageSize  types.Int64  `tfsdk:"storage_size"`
	StorageClass types.String `tfsdk:"storage_class"`
	AccessMode   types.String `tfsdk:"access_mode"`
	Attachments  types.Set    `tfsdk:"attachments"`
	Status       types.String `tfsdk:"status"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

func (d *volumeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

func (d *volumeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a volume in a Northflank project, either by its `id` or by its `name`. " +
			"Set exactly one of them. Mount paths are not available: the API does not report them.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the volume belongs to.",
			},
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Identifier of the volume. Set this to look the volume up by ID.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Name of the volume. Set this to look the volume up by name, which lists " +
					"the project's volumes to find it.",
			},
			"uid": computedString("Permanent identifier of the volume. Unlike `id`, it never changes and is never reused."),
			"storage_size": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Size of the volume, in megabytes.",
			},
			"storage_class": computedString("Storage class of the volume."),
			"access_mode":   computedString("`ReadWriteOnce` or `ReadWriteMany`."),
			"status":        computedString("Status of the volume on the cluster, such as `BOUND`."),
			"created_at":    computedString("Creation time, in RFC 3339 format."),
			"attachments": schema.SetNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Services and jobs the volume is attached to.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":   computedString("ID of the service or job."),
						"type": computedString("`service` or `job`."),
					},
				},
			},
		},
	}
}

func (d *volumeDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *volumeDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *volumeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config volumeDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := config.ProjectID.ValueString()
	volumeID := config.ID.ValueString()

	if config.ID.IsNull() {
		id, err := d.findVolumeIDByName(ctx, projectID, config.Name.ValueString())

		if err != nil {
			resp.Diagnostics.AddError("Error looking up Northflank volume", err.Error())
			return
		}

		volumeID = id
	}

	volume, err := getVolume(ctx, d.client, projectID, volumeID)

	if err != nil {
		summary := "Error reading Northflank volume"

		if isNotFound(err) {
			summary = "Northflank volume not found"
		}

		resp.Diagnostics.AddError(summary, fmt.Sprintf("Could not read volume %q in project %q: %s", volumeID, projectID, err))
		return
	}

	model, diags := volumeDataSourceModelFrom(ctx, projectID, volume)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, model)...)
}

// findVolumeIDByName walks the project's volumes and returns the ID of the one
// with that name.
func (d *volumeDataSource) findVolumeIDByName(ctx context.Context, projectID, name string) (string, error) {
	perPage := projectListPageSize
	params := &northflank.GetProjectsVolumesParams{PerPage: &perPage}

	var matches []string

	for {
		var page *northflank.GetProjectsVolumesHTTPResponse

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			got, err := d.client.GetProjectsVolumesWithResponse(ctx, projectID, params)

			if err != nil {
				return err
			}

			page = got
			return northflank.CheckResponse(got)
		})

		if err != nil {
			return "", fmt.Errorf("could not list volumes in project %q: %w", projectID, err)
		}

		if page.JSON200 == nil {
			return "", fmt.Errorf("could not list volumes in project %q: the API returned a successful status with no body", projectID)
		}

		for _, v := range page.JSON200.Data {
			if v.Name == name {
				matches = append(matches, v.Id)
			}
		}

		pagination := page.JSON200.Pagination

		if !pagination.HasNextPage || pagination.Cursor == nil {
			break
		}

		params.Cursor = pagination.Cursor
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no volume named %q exists in project %q", name, projectID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d volumes are named %q in project %q; look the volume up by id instead: %v",
			len(matches), name, projectID, matches)
	}
}

func volumeDataSourceModelFrom(ctx context.Context, projectID string, volume *volumeView) (*volumeDataSourceModel, diag.Diagnostics) {
	attachments, diags := volumeAttachmentsValue(ctx, volume.Attachments)

	return &volumeDataSourceModel{
		ID:           types.StringValue(volume.ID),
		ProjectID:    types.StringValue(projectID),
		Name:         types.StringValue(volume.Name),
		UID:          types.StringPointerValue(volume.UID),
		StorageSize:  types.Int64Value(int64(volume.StorageSize)),
		StorageClass: types.StringPointerValue(volume.StorageClass),
		AccessMode:   types.StringValue(volume.AccessMode),
		Attachments:  attachments,
		Status:       types.StringValue(volume.Status),
		CreatedAt:    timeValue(volume.CreatedAt),
	}, diags
}
