package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ datasource.DataSource                     = (*projectDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*projectDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*projectDataSource)(nil)
)

// projectListPageSize is the largest page the list endpoint allows.
const projectListPageSize = 100

type projectDataSource struct {
	client *northflank.ClientWithResponses
}

// NewProjectDataSource returns the northflank_project data source
// implementation.
func NewProjectDataSource() datasource.DataSource {
	return &projectDataSource{}
}

// projectDataSourceModel maps the northflank_project data source schema. id
// and name are optional+computed, because either can serve as the lookup key.
type projectDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	UID         types.String `tfsdk:"uid"`
	Description types.String `tfsdk:"description"`
	Region      types.String `tfsdk:"region"`
	ClusterID   types.String `tfsdk:"cluster_id"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

func (d *projectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

func (d *projectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a Northflank project in the token's team, either by its `id` or by its " +
			"`name`. Set exactly one of them.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Identifier of the project, such as `my-project`. Set this to look the project up by ID.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Name of the project. Set this to look the project up by name, which lists " +
					"the team's projects to find it.",
			},
			"uid": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Permanent identifier of the project. Unlike `id`, it never changes and is " +
					"never reused.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Short description of the project.",
			},
			"region": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Region the project is hosted in.",
			},
			"cluster_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cluster the project runs on: a bring-your-own-cloud cluster, or the managed cluster serving its region.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation time, in RFC 3339 format.",
			},
		},
	}
}

func (d *projectDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *projectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *projectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config projectDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := config.ID.ValueString()

	if config.ID.IsNull() {
		id, err := d.findProjectIDByName(ctx, config.Name.ValueString())

		if err != nil {
			resp.Diagnostics.AddError("Error looking up Northflank project", err.Error())
			return
		}

		projectID = id
	}

	project, err := getProject(ctx, d.client, projectID)

	if err != nil {
		summary := "Error reading Northflank project"

		if isNotFound(err) {
			summary = "Northflank project not found"
		}

		resp.Diagnostics.AddError(summary, fmt.Sprintf("Could not read project %q: %s", projectID, err))
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, projectDataSourceModelFrom(project))...)
}

// findProjectIDByName walks the project list, which reports names but not the
// rest of a project, and returns the ID of the one project with that name.
func (d *projectDataSource) findProjectIDByName(ctx context.Context, name string) (string, error) {
	perPage := projectListPageSize
	params := &northflank.GetProjectsParams{PerPage: &perPage}

	var matches []string

	for {
		var page *northflank.GetProjectsHTTPResponse

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			got, err := d.client.GetProjectsWithResponse(ctx, params)

			if err != nil {
				return err
			}

			page = got
			return northflank.CheckResponse(got)
		})

		if err != nil {
			return "", fmt.Errorf("could not list projects: %w", err)
		}

		if page.JSON200 == nil {
			return "", fmt.Errorf("could not list projects: the API returned a successful status with no body")
		}

		for _, p := range page.JSON200.Data.Projects {
			if p.Name == name {
				matches = append(matches, p.Id)
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
		return "", fmt.Errorf("no project named %q exists in the team this token belongs to", name)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d projects are named %q; look the project up by id instead: %v", len(matches), name, matches)
	}
}

func projectDataSourceModelFrom(project *projectView) *projectDataSourceModel {
	return &projectDataSourceModel{
		ID:          types.StringValue(project.ID),
		Name:        types.StringValue(project.Name),
		UID:         types.StringValue(project.UID),
		Description: types.StringPointerValue(project.Description),
		Region:      types.StringValue(project.Region),
		ClusterID:   types.StringValue(project.ClusterID),
		CreatedAt:   timeValue(project.CreatedAt),
	}
}
