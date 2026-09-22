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
	_ datasource.DataSource                     = (*secretGroupDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*secretGroupDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*secretGroupDataSource)(nil)
)

type secretGroupDataSource struct {
	client *northflank.ClientWithResponses
}

// NewSecretGroupDataSource returns the northflank_secret_group data source
// implementation.
func NewSecretGroupDataSource() datasource.DataSource {
	return &secretGroupDataSource{}
}

// secretGroupDataSourceModel maps the northflank_secret_group data source
// schema. id and name are optional+computed, because either can serve as the
// lookup key.
type secretGroupDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	ProjectID    types.String `tfsdk:"project_id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	Priority     types.Int64  `tfsdk:"priority"`
	SecretType   types.String `tfsdk:"secret_type"`
	Type         types.String `tfsdk:"type"`
	Tags         types.Set    `tfsdk:"tags"`
	Restrictions types.Object `tfsdk:"restrictions"`
	Variables    types.Map    `tfsdk:"variables"`
	CreatedAt    types.String `tfsdk:"created_at"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
}

func (d *secretGroupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret_group"
}

func (d *secretGroupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a secret group in a Northflank project, either by its `id` or by its " +
			"`name`. Set exactly one of them.\n\n" +
			"`variables` holds the group's own decrypted values, not those inherited from linked addons, " +
			"and ends up in Terraform state.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the secret group belongs to.",
			},
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Identifier of the secret group. Set this to look the group up by ID.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Name of the secret group. Set this to look the group up by name, which lists " +
					"the project's secret groups to find it.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Short description of the secret group.",
			},
			"priority": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Priority used to merge secret groups that define the same key.",
			},
			"secret_type": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Where the variables are injected: `environment-arguments`, `environment` or `arguments`.",
			},
			"type": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Permission type of the group: `secret` or `config`.",
			},
			"tags": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Tags attached to the secret group.",
			},
			"variables": schema.MapAttribute{
				Computed:            true,
				Sensitive:           true,
				ElementType:         types.StringType,
				MarkdownDescription: "The group's secret variables, as a map of name to decrypted value.",
			},
			"restrictions": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Which resources inherit the secret group. Null when it is not restricted.",
				Attributes: map[string]schema.Attribute{
					"restricted": schema.BoolAttribute{
						Computed:            true,
						MarkdownDescription: "Whether the restrictions apply.",
					},
					"resources": schema.SetNestedAttribute{
						Computed:            true,
						MarkdownDescription: "Services and jobs the secret group is restricted to.",
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"id": schema.StringAttribute{
									Computed:            true,
									MarkdownDescription: "ID of the service or job.",
								},
								"type": schema.StringAttribute{
									Computed:            true,
									MarkdownDescription: "Kind of resource: `service` or `job`.",
								},
							},
						},
					},
					"tags": schema.SetAttribute{
						Computed:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Tags a resource must carry to inherit the secret group.",
					},
					"tag_match_condition": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "Whether a resource needs `and` all of `tags`, or `or` any one of them.",
					},
					"stage_ids": schema.SetAttribute{
						Computed:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "IDs of the environments the secret group is restricted to.",
					},
				},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation time, in RFC 3339 format.",
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Time of the last update, in RFC 3339 format.",
			},
		},
	}
}

func (d *secretGroupDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *secretGroupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *secretGroupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config secretGroupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := config.ProjectID.ValueString()
	secretID := config.ID.ValueString()

	if config.ID.IsNull() {
		id, err := d.findSecretGroupIDByName(ctx, projectID, config.Name.ValueString())

		if err != nil {
			resp.Diagnostics.AddError("Error looking up Northflank secret group", err.Error())
			return
		}

		secretID = id
	}

	group, err := getSecretGroup(ctx, d.client, projectID, secretID)

	if err != nil {
		summary := "Error reading Northflank secret group"

		if isNotFound(err) {
			summary = "Northflank secret group not found"
		}

		resp.Diagnostics.AddError(summary,
			fmt.Sprintf("Could not read secret group %q in project %q: %s", secretID, projectID, err))
		return
	}

	model, diags := secretGroupDataSourceModelFrom(ctx, group)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, model)...)
}

// findSecretGroupIDByName walks the project's secret groups and returns the ID
// of the one with that name.
func (d *secretGroupDataSource) findSecretGroupIDByName(ctx context.Context, projectID, name string) (string, error) {
	perPage := projectListPageSize
	params := &northflank.GetProjectsSecretsParams{PerPage: &perPage}

	var matches []string

	for {
		var page *northflank.GetProjectsSecretsHTTPResponse

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			got, err := d.client.GetProjectsSecretsWithResponse(ctx, projectID, params)

			if err != nil {
				return err
			}

			page = got
			return northflank.CheckResponse(got)
		})

		if err != nil {
			return "", fmt.Errorf("could not list secret groups in project %q: %w", projectID, err)
		}

		if page.JSON200 == nil {
			return "", fmt.Errorf("could not list secret groups in project %q: the API returned a successful status with no body", projectID)
		}

		for _, s := range page.JSON200.Data.Secrets {
			if s.Name == name {
				matches = append(matches, s.Id)
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
		return "", fmt.Errorf("no secret group named %q exists in project %q", name, projectID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d secret groups are named %q in project %q; look the group up by id instead: %v",
			len(matches), name, projectID, matches)
	}
}

// secretGroupDataSourceModelFrom reuses the resource's mapping, so the two
// report every attribute the same way.
func secretGroupDataSourceModelFrom(ctx context.Context, group *secretGroupView) (*secretGroupDataSourceModel, diag.Diagnostics) {
	var r secretGroupResourceModel
	diags := applySecretGroup(ctx, &r, group)

	return &secretGroupDataSourceModel{
		ID:           r.ID,
		ProjectID:    r.ProjectID,
		Name:         r.Name,
		Description:  r.Description,
		Priority:     r.Priority,
		SecretType:   r.SecretType,
		Type:         r.Type,
		Tags:         r.Tags,
		Restrictions: r.Restrictions,
		Variables:    r.Variables,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}, diags
}
