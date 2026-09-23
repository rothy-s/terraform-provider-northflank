package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
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
	_ resource.Resource                = (*subdomainResource)(nil)
	_ resource.ResourceWithConfigure   = (*subdomainResource)(nil)
	_ resource.ResourceWithImportState = (*subdomainResource)(nil)
)

type subdomainResource struct {
	client *northflank.ClientWithResponses
}

// NewSubdomainResource returns the northflank_subdomain resource
// implementation.
func NewSubdomainResource() resource.Resource {
	return &subdomainResource{}
}

// subdomainResourceModel maps the northflank_subdomain schema.
type subdomainResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Domain      types.String `tfsdk:"domain"`
	Name        types.String `tfsdk:"name"`
	RoutingMode types.String `tfsdk:"routing_mode"`
	FullName    types.String `tfsdk:"full_name"`
	RecordType  types.String `tfsdk:"record_type"`
	RecordValue types.String `tfsdk:"record_value"`
	Verified    types.Bool   `tfsdk:"verified"`
}

func (r *subdomainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_subdomain"
}

func (r *subdomainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	computed := func(description string) schema.StringAttribute {
		return schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: description,
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Adds a subdomain, such as `api` for `api.example.com`, to a verified domain.\n\n" +
			"Create a DNS record of type `record_type` named `full_name` with the value `record_value`, then " +
			"verify with `northflank_subdomain_verification`. Once verified, route it to a service by listing " +
			"`full_name` in that service's `ports[].domains`.",
		Attributes: map[string]schema.Attribute{
			"id": computed("`<domain>/<name>`."),
			"domain": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Root domain to add the subdomain to. It must be verified first, so pass " +
					"`northflank_domain_verification.<name>.domain` to get the ordering right. Changing this " +
					"forces a new subdomain.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(domainNamePattern, "must be a domain name such as example.com"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Subdomain to add, such as `api` or `eu.api`, `@` for the domain itself, or " +
					"`*` for a wildcard. Changing this forces a new subdomain.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(subdomainPattern, "must be a lowercase subdomain, @ or *"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"routing_mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("paths"),
				MarkdownDescription: "How traffic is routed: `paths` (the default), `geoRouting` or " +
					"`loadBalancerSubdomain`. Northflank cannot change it later, so changing it forces a new " +
					"subdomain.",
				Validators: []validator.String{
					stringvalidator.OneOf(subdomainRoutingModes...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"full_name":    computed("Full name of the subdomain, such as `api.example.com`. This is what `ports[].domains` takes."),
			"record_type":  computed("Type of the DNS record to create, `CNAME` for subdomains."),
			"record_value": computed("Value of the DNS record to create."),
			"verified": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether Northflank has verified the DNS record.",
			},
		},
	}
}

func (r *subdomainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *subdomainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan subdomainResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	domain, name := plan.Domain.ValueString(), plan.Name.ValueString()
	body, _ := json.Marshal(map[string]string{"subdomain": name, "routingMode": plan.RoutingMode.ValueString()})

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostDomainsDomainSubdomainsWithBodyWithResponse(ctx, domain, "application/json", bytes.NewReader(body))

	if err == nil {
		err = northflank.CheckResponse(created)
	}

	if err != nil {
		resp.Diagnostics.AddError(
			"Error adding Northflank subdomain",
			fmt.Sprintf("Could not add %q to %q: %s. The domain must be verified first.", name, domain, err),
		)
		return
	}

	tflog.Debug(ctx, "added Northflank subdomain", map[string]any{"domain": domain, "subdomain": name})

	read, err := decodeData[subdomainRead](created.Body, "subdomain")

	if err != nil {
		plan.ID = types.StringValue(domain + "/" + name)
		plan.FullName, plan.RecordType, plan.RecordValue = types.StringNull(), types.StringNull(), types.StringNull()
		plan.Verified = types.BoolValue(false)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading added Northflank subdomain",
			fmt.Sprintf("%q was added to %q but the response could not be read: %s. It has been saved to "+
				"state; the DNS record is filled in on the next refresh.", name, domain, err),
		)
		return
	}

	applySubdomain(&plan, domain, name, read)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subdomainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state subdomainResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	domain, name := state.Domain.ValueString(), state.Name.ValueString()
	read, err := getSubdomain(ctx, r.client, domain, name)

	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError("Error reading Northflank subdomain", err.Error())
		return
	}

	applySubdomain(&state, domain, name, read)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never called with a change: every configurable attribute forces
// a replacement.
func (r *subdomainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan subdomainResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subdomainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state subdomainResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	domain, name := state.Domain.ValueString(), state.Name.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteDomainsDomainSubdomainsSubdomainWithResponse(ctx, domain, name)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(deleted)
	})

	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError(
			"Error deleting Northflank subdomain",
			fmt.Sprintf("Could not remove %q from %q: %s. A subdomain still routed to a service may need "+
				"removing from its ports first.", name, domain, err),
		)
	}
}

func (r *subdomainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	domain, name, err := parseSubdomainID(req.ID)

	if err != nil {
		resp.Diagnostics.AddError("Invalid import identifier", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain"), domain)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// applySubdomain copies an API response onto the model. The name stays as
// configured: the API may report the apex under another spelling.
func applySubdomain(model *subdomainResourceModel, domain, name string, read *subdomainRead) {
	model.ID = types.StringValue(domain + "/" + name)
	model.FullName = types.StringValue(read.FullName)
	model.RecordType = types.StringValue(read.RecordType)
	model.RecordValue = types.StringValue(read.Content)
	model.Verified = types.BoolValue(read.Verified)

	if read.RoutingMode != "" {
		model.RoutingMode = types.StringValue(read.RoutingMode)
	}
}
