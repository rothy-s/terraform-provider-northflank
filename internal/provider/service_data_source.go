package provider

import (
	"cmp"
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
	_ datasource.DataSource                     = (*serviceDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*serviceDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*serviceDataSource)(nil)
)

type serviceDataSource struct {
	client *northflank.ClientWithResponses
}

// NewServiceDataSource returns the northflank_service data source
// implementation. It reads services of every kind, since the API shares one
// endpoint for them.
func NewServiceDataSource() datasource.DataSource {
	return &serviceDataSource{}
}

// serviceDataSourceModel maps the northflank_service data source schema. It
// deliberately leaves out the runtime environment, which a lookup should not
// copy into state.
type serviceDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	ProjectID      types.String `tfsdk:"project_id"`
	Name           types.String `tfsdk:"name"`
	UID            types.String `tfsdk:"uid"`
	AppID          types.String `tfsdk:"app_id"`
	ServiceType    types.String `tfsdk:"service_type"`
	Description    types.String `tfsdk:"description"`
	Tags           types.Set    `tfsdk:"tags"`
	DeploymentPlan types.String `tfsdk:"deployment_plan"`
	Instances      types.Int64  `tfsdk:"instances"`
	ImagePath      types.String `tfsdk:"image_path"`
	BuildServiceID types.String `tfsdk:"build_service_id"`
	RepositoryURL  types.String `tfsdk:"repository_url"`
	Branch         types.String `tfsdk:"branch"`
	Command        types.String `tfsdk:"command"`
	Entrypoint     types.String `tfsdk:"entrypoint"`
	Ports          types.List   `tfsdk:"ports"`
	CreatedAt      types.String `tfsdk:"created_at"`
}

func (d *serviceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_service"
}

func (d *serviceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a Northflank service of any kind -- deployment, combined or build -- " +
			"either by its `id` or by its `name`. Set exactly one of them.\n\n" +
			"Attributes that do not apply to the service's kind are null: a build service has no instances or " +
			"ports. The runtime environment is not exposed.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the service belongs to.",
			},
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Identifier of the service. Set this to look the service up by ID.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Name of the service. Set this to look the service up by name, which lists " +
					"the project's services to find it.",
			},
			"uid":              computedString("Permanent identifier of the service. Unlike `id`, it never changes and is never reused."),
			"app_id":           computedString("Full identifier Northflank uses for the service's deployment."),
			"service_type":     computedString("`deployment`, `combined` or `build`."),
			"description":      computedString("Short description of the service."),
			"deployment_plan":  computedString("ID of the compute plan the service runs on."),
			"image_path":       computedString("Image a deployment service runs, when it runs an external image."),
			"build_service_id": computedString("Build service a deployment service runs the output of, when it does."),
			"repository_url":   computedString("Git repository a build or combined service builds from."),
			"branch":           computedString("Branch a combined service builds and deploys. Null for other kinds."),
			"command":          computedString("Command override. Null when the image's `CMD` is used."),
			"entrypoint":       computedString("Entrypoint override. Null when the image's `ENTRYPOINT` is used."),
			"created_at":       computedString("Creation time, in RFC 3339 format."),
			"tags": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Tags attached to the service.",
			},
			"instances": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Number of instances. Null for a build service.",
			},
			"ports": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Ports the service listens on. Empty for a build service.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": computedString("Name identifying the port."),
						"internal_port": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Port number the container listens on.",
						},
						"protocol": computedString("`HTTP`, `HTTP/2`, `TCP` or `UDP`."),
						"public": schema.BoolAttribute{
							Computed:            true,
							MarkdownDescription: "Whether the port is exposed to the internet.",
						},
						"domains": schema.SetAttribute{
							Computed:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "Custom domains routed to this port.",
						},
						"disable_nf_domain": schema.BoolAttribute{
							Computed:            true,
							MarkdownDescription: "Whether the port's default `code.run` domain is turned off.",
						},
					},
				},
			},
		},
	}
}

func (d *serviceDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *serviceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *serviceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config serviceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := config.ProjectID.ValueString()
	serviceID := config.ID.ValueString()

	if config.ID.IsNull() {
		id, err := d.findServiceIDByName(ctx, projectID, config.Name.ValueString())

		if err != nil {
			resp.Diagnostics.AddError("Error looking up Northflank service", err.Error())
			return
		}

		serviceID = id
	}

	service, err := getService(ctx, d.client, projectID, serviceID)

	if err != nil {
		summary := "Error reading Northflank service"

		if isNotFound(err) {
			summary = "Northflank service not found"
		}

		resp.Diagnostics.AddError(summary, fmt.Sprintf("Could not read service %q in project %q: %s", serviceID, projectID, err))
		return
	}

	model, diags := serviceDataSourceModelFrom(ctx, projectID, service)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, model)...)
}

// findServiceIDByName walks the project's services and returns the ID of the
// one with that name.
func (d *serviceDataSource) findServiceIDByName(ctx context.Context, projectID, name string) (string, error) {
	perPage := projectListPageSize
	params := &northflank.GetProjectsServicesParams{PerPage: &perPage}

	var matches []string

	for {
		var page *northflank.GetProjectsServicesHTTPResponse

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			got, err := d.client.GetProjectsServicesWithResponse(ctx, projectID, params)

			if err != nil {
				return err
			}

			page = got
			return northflank.CheckResponse(got)
		})

		if err != nil {
			return "", fmt.Errorf("could not list services in project %q: %w", projectID, err)
		}

		if page.JSON200 == nil {
			return "", fmt.Errorf("could not list services in project %q: the API returned a successful status with no body", projectID)
		}

		for _, s := range page.JSON200.Data.Services {
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
		return "", fmt.Errorf("no service named %q exists in project %q", name, projectID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d services are named %q in project %q; look the service up by id instead: %v",
			len(matches), name, projectID, matches)
	}
}

func serviceDataSourceModelFrom(ctx context.Context, projectID string, service *serviceRead) (*serviceDataSourceModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	tags, d := types.SetValueFrom(ctx, types.StringType, nonNil(service.Tags))
	diags.Append(d...)

	ports, d := portsValue(ctx, service.Ports, types.ListNull(types.ObjectType{AttrTypes: servicePortAttrTypes}))
	diags.Append(d...)

	model := &serviceDataSourceModel{
		ID:             types.StringValue(service.ID),
		ProjectID:      types.StringValue(projectID),
		Name:           types.StringValue(service.Name),
		UID:            types.StringValue(service.UID),
		AppID:          types.StringValue(service.AppID),
		ServiceType:    types.StringValue(service.ServiceType),
		Description:    types.StringPointerValue(service.Description),
		Tags:           tags,
		DeploymentPlan: types.StringValue(service.Billing.DeploymentPlan),
		Instances:      types.Int64Null(),
		ImagePath:      types.StringNull(),
		BuildServiceID: types.StringNull(),
		RepositoryURL:  types.StringNull(),
		Branch:         types.StringNull(),
		Command:        types.StringPointerValue(service.command()),
		Entrypoint:     types.StringPointerValue(service.entrypoint()),
		Ports:          ports,
		CreatedAt:      timePointerValue(service.CreatedAt),
	}

	if vcs := service.VcsData; vcs != nil {
		model.RepositoryURL = types.StringValue(vcs.ProjectURL)
		model.Branch = types.StringPointerValue(vcs.ProjectBranch)
	}

	if dep := service.Deployment; dep != nil {
		model.Instances = types.Int64Value(int64(dep.Instances))

		if dep.External != nil {
			model.ImagePath = types.StringValue(dep.External.ImagePath)
		}

		if dep.Internal != nil {
			model.BuildServiceID = types.StringValue(cmp.Or(dep.Internal.ID, dep.Internal.NfObjectID))
		}
	}

	return model, diags
}
