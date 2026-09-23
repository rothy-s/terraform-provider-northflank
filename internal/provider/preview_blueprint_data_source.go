package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ datasource.DataSource                     = (*previewBlueprintDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*previewBlueprintDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*previewBlueprintDataSource)(nil)
)

type previewBlueprintDataSource struct {
	client *northflank.ClientWithResponses
}

// NewPreviewBlueprintDataSource returns the northflank_preview_blueprint data
// source implementation.
func NewPreviewBlueprintDataSource() datasource.DataSource {
	return &previewBlueprintDataSource{}
}

// previewBlueprintDataSourceModel maps the data source schema. The template
// is left out: it may carry references to secrets.
type previewBlueprintDataSourceModel struct {
	ID        types.String `tfsdk:"id"`
	ProjectID types.String `tfsdk:"project_id"`
	Name      types.String `tfsdk:"name"`
	Status    types.String `tfsdk:"status"`
	Paused    types.Bool   `tfsdk:"paused"`
}

func (d *previewBlueprintDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_preview_blueprint"
}

func (d *previewBlueprintDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a preview blueprint in a project, by `id` or by `name`. Set exactly one " +
			"of them. Useful for handing the blueprint's ID to a CI pipeline that starts runs through the API.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the blueprint belongs to.",
			},
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Identifier of the blueprint. Set this to look it up by ID.",
			},
			"name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Name of the blueprint. Set this to look it up by name.",
			},
			"status": computedString("Status of the blueprint's latest run."),
			"paused": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the blueprint's Git and webhook triggers are paused.",
			},
		},
	}
}

func (d *previewBlueprintDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *previewBlueprintDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *previewBlueprintDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config previewBlueprintDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := config.ProjectID.ValueString()
	blueprintID := config.ID.ValueString()

	if config.ID.IsNull() {
		id, err := findPreviewBlueprintID(ctx, d.client, projectID, config.Name.ValueString())

		if err != nil {
			resp.Diagnostics.AddError("Error looking up Northflank preview blueprint", err.Error())
			return
		}

		blueprintID = id
	}

	read, err := getPreviewBlueprint(ctx, d.client, projectID, blueprintID)

	if err != nil {
		resp.Diagnostics.AddError("Error reading Northflank preview blueprint",
			fmt.Sprintf("Could not read preview blueprint %q in project %q: %s", blueprintID, projectID, err))
		return
	}

	var model previewBlueprintModel
	applyPreviewBlueprint(&model, read)

	resp.Diagnostics.Append(resp.State.Set(ctx, &previewBlueprintDataSourceModel{
		ID:        types.StringValue(blueprintID),
		ProjectID: types.StringValue(projectID),
		Name:      model.Name,
		Status:    model.Status,
		Paused:    model.Paused,
	})...)
}
