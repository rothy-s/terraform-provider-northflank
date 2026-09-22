// Package provider implements the Terraform provider for the Northflank API.
package provider

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Environment variables consulted when the matching provider attribute is not
// set in configuration.
const (
	envToken    = "NORTHFLANK_API_TOKEN"
	envEndpoint = "NORTHFLANK_ENDPOINT"
)

// Ensure the implementation satisfies the framework interfaces.
var _ provider.Provider = (*northflankProvider)(nil)

type northflankProvider struct {
	// version is set at build time and reported to Terraform in the user agent.
	version string
}

// New returns a function that constructs the provider, as expected by
// providerserver.Serve.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &northflankProvider{version: version}
	}
}

// providerData is what resources and data sources receive from Configure.
type providerData struct {
	Client *northflank.ClientWithResponses
}

// providerDataFrom pulls the configured client out of what the framework hands
// a resource or data source. A nil value means the provider has not been
// configured yet, and the framework will call Configure again.
func providerDataFrom(raw any, diags *diag.Diagnostics) (*providerData, bool) {
	if raw == nil {
		return nil, false
	}

	data, ok := raw.(*providerData)

	if !ok {
		diags.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *providerData, got %T. This is a bug in the provider.", raw),
		)

		return nil, false
	}

	return data, true
}

// providerModel maps the provider configuration block.
type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

func (p *northflankProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "northflank"
	resp.Version = p.version
}

func (p *northflankProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages [Northflank](https://northflank.com) resources through the " +
			"[Northflank API](https://northflank.com/docs/v1/api/introduction).",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Base URL of the Northflank API, without the `/v1` suffix. Defaults to `" +
					northflank.DefaultBaseURL + "`, or the `" + envEndpoint + "` environment variable when set.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "API token used to authenticate against the Northflank API. Tokens belong " +
					"to a team and carry the permissions of the API role they were created from. May also be " +
					"set with the `" + envToken + "` environment variable.",
			},
		},
	}
}

func (p *northflankProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// Values that are still unknown at this point come from other resources'
	// attributes, which are not resolved yet. Terraform will call Configure
	// again once they are, so defer rather than reading a bogus value.
	if config.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Unknown Northflank API endpoint",
			"The endpoint cannot be determined at configuration time. Either set it to a static value or use the "+
				envEndpoint+" environment variable.",
		)
	}

	if config.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Unknown Northflank API token",
			"The token cannot be determined at configuration time. Either set it to a static value or use the "+
				envToken+" environment variable.",
		)
	}

	if resp.Diagnostics.HasError() {
		return
	}

	// Configuration wins over the environment.
	endpoint := os.Getenv(envEndpoint)

	if !config.Endpoint.IsNull() {
		endpoint = config.Endpoint.ValueString()
	}

	if endpoint == "" {
		endpoint = northflank.DefaultBaseURL
	}

	token := os.Getenv(envToken)

	if !config.Token.IsNull() {
		token = config.Token.ValueString()
	}

	if token == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Missing Northflank API token",
			"An API token is required to talk to the Northflank API. Set the provider `token` attribute or the "+
				envToken+" environment variable. Tokens are created in the Northflank dashboard under team "+
				"settings, API, Tokens.",
		)
		return
	}

	client, err := northflank.NewAuthenticated(endpoint, token)

	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to create Northflank API client",
			"An unexpected error occurred while constructing the API client: "+err.Error(),
		)
		return
	}

	data := &providerData{Client: client}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *northflankProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewProjectResource,
		NewSecretGroupResource,
		NewJobResource,
		NewDeploymentServiceResource,
		NewBuildServiceResource,
		NewCombinedServiceResource,
		NewVolumeResource,
	}
}

func (p *northflankProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewProjectDataSource,
		NewSecretGroupDataSource,
		NewJobDataSource,
		NewServiceDataSource,
		NewVolumeDataSource,
	}
}

func (p *northflankProvider) Functions(_ context.Context) []func() function.Function {
	return nil
}
