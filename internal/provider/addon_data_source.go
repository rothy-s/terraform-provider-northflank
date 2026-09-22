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
	_ datasource.DataSource                     = (*addonDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*addonDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*addonDataSource)(nil)
)

type addonDataSource struct {
	client *northflank.ClientWithResponses
}

// NewAddonDataSource returns the northflank_addon data source implementation.
func NewAddonDataSource() datasource.DataSource {
	return &addonDataSource{}
}

// addonDataSourceModel maps the northflank_addon data source schema.
type addonDataSourceModel struct {
	ID                types.String `tfsdk:"id"`
	ProjectID         types.String `tfsdk:"project_id"`
	Name              types.String `tfsdk:"name"`
	UID               types.String `tfsdk:"uid"`
	AppID             types.String `tfsdk:"app_id"`
	Description       types.String `tfsdk:"description"`
	Tags              types.Set    `tfsdk:"tags"`
	Type              types.String `tfsdk:"type"`
	Version           types.String `tfsdk:"version"`
	DeploymentPlan    types.String `tfsdk:"deployment_plan"`
	Replicas          types.Int64  `tfsdk:"replicas"`
	StorageSize       types.Int64  `tfsdk:"storage_size"`
	StorageClass      types.String `tfsdk:"storage_class"`
	TLSEnabled        types.Bool   `tfsdk:"tls_enabled"`
	ExternalAccess    types.Bool   `tfsdk:"external_access"`
	VpcAccessible     types.Bool   `tfsdk:"vpc_accessible"`
	Status            types.String `tfsdk:"status"`
	Region            types.String `tfsdk:"region"`
	Secrets           types.Map    `tfsdk:"secrets"`
	ConnectionDetails types.Map    `tfsdk:"connection_details"`
	CreatedAt         types.String `tfsdk:"created_at"`
}

func (d *addonDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_addon"
}

func (d *addonDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	computedBool := func(description string) schema.BoolAttribute {
		return schema.BoolAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an addon in a Northflank project, either by its `id` or by its `name`, " +
			"including its connection details. Set exactly one of `id` and `name`.\n\n" +
			"`secrets` and `connection_details` end up in Terraform state, and are empty while the addon is " +
			"not running.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the addon belongs to.",
			},
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Identifier of the addon. Set this to look the addon up by ID.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Name of the addon. Set this to look the addon up by name, which lists the " +
					"project's addons to find it.",
			},
			"uid":             computedString("Permanent identifier of the addon. Unlike `id`, it never changes and is never reused."),
			"app_id":          computedString("Full identifier Northflank uses for the addon."),
			"description":     computedString("Short description of the addon."),
			"type":            computedString("Kind of addon, such as `postgresql`."),
			"version":         computedString("Version the addon runs."),
			"deployment_plan": computedString("ID of the compute plan each replica uses."),
			"storage_class":   computedString("Storage class of the addon."),
			"status":          computedString("State of the addon, such as `running`."),
			"region":          computedString("Region the addon is deployed in."),
			"created_at":      computedString("Creation time, in RFC 3339 format."),
			"tls_enabled":     computedBool("Whether connections use TLS."),
			"external_access": computedBool("Whether the addon is reachable from the internet."),
			"vpc_accessible":  computedBool("Whether the addon is exposed on the cluster's private load balancer."),
			"tags": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Tags attached to the addon.",
			},
			"replicas": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Number of replicas.",
			},
			"storage_size": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Storage per replica, in megabytes.",
			},
			"secrets": schema.MapAttribute{
				Computed:            true,
				Sensitive:           true,
				ElementType:         types.StringType,
				MarkdownDescription: "Secrets for connecting, such as `username` and `password`.",
			},
			"connection_details": schema.MapAttribute{
				Computed:            true,
				Sensitive:           true,
				ElementType:         types.StringType,
				MarkdownDescription: "Connection details such as hosts, ports and connection strings.",
			},
		},
	}
}

func (d *addonDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *addonDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *addonDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config addonDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := config.ProjectID.ValueString()
	addonID := config.ID.ValueString()

	if config.ID.IsNull() {
		id, err := d.findAddonIDByName(ctx, projectID, config.Name.ValueString())

		if err != nil {
			resp.Diagnostics.AddError("Error looking up Northflank addon", err.Error())
			return
		}

		addonID = id
	}

	addon, err := getAddon(ctx, d.client, projectID, addonID)

	if err != nil {
		summary := "Error reading Northflank addon"

		if isNotFound(err) {
			summary = "Northflank addon not found"
		}

		resp.Diagnostics.AddError(summary, fmt.Sprintf("Could not read addon %q in project %q: %s", addonID, projectID, err))
		return
	}

	// Credentials are only there while the addon runs; without them the
	// lookup still succeeds, with the maps empty.
	creds, err := getAddonCredentials(ctx, d.client, projectID, addonID)

	if err != nil {
		creds = nil
	}

	model, diags := addonDataSourceModelFrom(ctx, projectID, addon, creds)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, model)...)
}

// findAddonIDByName walks the project's addons and returns the ID of the one
// with that name.
func (d *addonDataSource) findAddonIDByName(ctx context.Context, projectID, name string) (string, error) {
	perPage := projectListPageSize
	params := &northflank.GetProjectsAddonsParams{PerPage: &perPage}

	var matches []string

	for {
		var page *northflank.GetProjectsAddonsHTTPResponse

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			got, err := d.client.GetProjectsAddonsWithResponse(ctx, projectID, params)

			if err != nil {
				return err
			}

			page = got
			return northflank.CheckResponse(got)
		})

		if err != nil {
			return "", fmt.Errorf("could not list addons in project %q: %w", projectID, err)
		}

		if page.JSON200 == nil {
			return "", fmt.Errorf("could not list addons in project %q: the API returned a successful status with no body", projectID)
		}

		for _, a := range page.JSON200.Data.Addons {
			if a.Name == name {
				matches = append(matches, a.Id)
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
		return "", fmt.Errorf("no addon named %q exists in project %q", name, projectID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d addons are named %q in project %q; look the addon up by id instead: %v",
			len(matches), name, projectID, matches)
	}
}

func addonDataSourceModelFrom(ctx context.Context, projectID string, addon *addonRead, creds *addonCredentials) (*addonDataSourceModel, diag.Diagnostics) {
	// Reuse the resource's mapping so both report attributes the same way.
	var r addonResourceModel
	diags := applyAddon(ctx, &r, addon)

	secrets, envs, d := credentialMaps(ctx, creds)
	diags.Append(d...)

	return &addonDataSourceModel{
		ID:                r.ID,
		ProjectID:         types.StringValue(projectID),
		Name:              r.Name,
		UID:               r.UID,
		AppID:             r.AppID,
		Description:       r.Description,
		Tags:              r.Tags,
		Type:              r.Type,
		Version:           r.RunningVersion,
		DeploymentPlan:    r.DeploymentPlan,
		Replicas:          r.Replicas,
		StorageSize:       r.StorageSize,
		StorageClass:      r.StorageClass,
		TLSEnabled:        r.TLSEnabled,
		ExternalAccess:    r.ExternalAccess,
		VpcAccessible:     r.VpcAccessible,
		Status:            r.Status,
		Region:            r.Region,
		Secrets:           secrets,
		ConnectionDetails: envs,
		CreatedAt:         r.CreatedAt,
	}, diags
}
