package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ datasource.DataSource              = (*domainDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*domainDataSource)(nil)
)

type domainDataSource struct {
	client *northflank.ClientWithResponses
}

// NewDomainDataSource returns the northflank_domain data source
// implementation.
func NewDomainDataSource() datasource.DataSource {
	return &domainDataSource{}
}

// domainDataSourceModel maps the northflank_domain data source schema.
type domainDataSourceModel struct {
	Domain                  types.String `tfsdk:"domain"`
	Status                  types.String `tfsdk:"status"`
	VerificationRecordName  types.String `tfsdk:"verification_record_name"`
	VerificationRecordValue types.String `tfsdk:"verification_record_value"`
	Subdomains              types.List   `tfsdk:"subdomains"`
}

var domainSubdomainAttrTypes = map[string]attr.Type{
	"name":      types.StringType,
	"full_name": types.StringType,
}

func (d *domainDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain"
}

func (d *domainDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a root domain registered with the team, and the subdomains added to it.",
		Attributes: map[string]schema.Attribute{
			"domain": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Root domain, such as `example.com`.",
			},
			"status":                    computedString("`pending` or `verified`."),
			"verification_record_name":  computedString("Name of the TXT record that proves ownership."),
			"verification_record_value": computedString("Value of the TXT record that proves ownership."),
			"subdomains": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Subdomains added to the domain.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":      computedString("The subdomain, as Northflank names it."),
						"full_name": computedString("Full name, such as `api.example.com`."),
					},
				},
			},
		},
	}
}

func (d *domainDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *domainDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config domainDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	domain := config.Domain.ValueString()
	read, err := getDomain(ctx, d.client, domain)

	if err != nil {
		summary := "Error reading Northflank domain"

		if isNotFound(err) {
			summary = "Northflank domain not found"
		}

		resp.Diagnostics.AddError(summary, fmt.Sprintf("Could not read %q: %s", domain, err))
		return
	}

	subdomains := make([]attr.Value, 0, len(read.Subdomains))

	for _, s := range read.Subdomains {
		subdomains = append(subdomains, types.ObjectValueMust(domainSubdomainAttrTypes, map[string]attr.Value{
			"name":      types.StringValue(s.Name),
			"full_name": types.StringValue(s.FullName),
		}))
	}

	list, diags := types.ListValue(types.ObjectType{AttrTypes: domainSubdomainAttrTypes}, subdomains)
	resp.Diagnostics.Append(diags...)

	resp.Diagnostics.Append(resp.State.Set(ctx, &domainDataSourceModel{
		Domain:                  types.StringValue(read.Name),
		Status:                  types.StringValue(read.Status),
		VerificationRecordName:  types.StringPointerValue(read.Hostname),
		VerificationRecordValue: types.StringPointerValue(read.Token),
		Subdomains:              list,
	})...)
}
