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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ resource.Resource                = (*domainResource)(nil)
	_ resource.ResourceWithConfigure   = (*domainResource)(nil)
	_ resource.ResourceWithImportState = (*domainResource)(nil)
)

type domainResource struct {
	client *northflank.ClientWithResponses
}

// NewDomainResource returns the northflank_domain resource implementation.
func NewDomainResource() resource.Resource {
	return &domainResource{}
}

// domainResourceModel maps the northflank_domain schema.
type domainResourceModel struct {
	ID                      types.String `tfsdk:"id"`
	Domain                  types.String `tfsdk:"domain"`
	VerificationRecordName  types.String `tfsdk:"verification_record_name"`
	VerificationRecordValue types.String `tfsdk:"verification_record_value"`
	Status                  types.String `tfsdk:"status"`
}

func (r *domainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain"
}

func (r *domainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Registers a root domain with the team, such as `example.com`, so that its " +
			"subdomains can route to services.\n\n" +
			"Registering does not verify ownership. Create a TXT record named `verification_record_name` with " +
			"the value `verification_record_value`, then verify with `northflank_domain_verification`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The domain name, which is also its identifier.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"domain": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Root domain to register, such as `example.com`. Changing this registers a new domain.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(domainNamePattern, "must be a domain name such as example.com"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"verification_record_name": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Full DNS name of the TXT record to create to prove ownership, such as `verify-abc123.example.com`. " +
					"With a DNS provider that takes names relative to the zone, strip the zone from it.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"verification_record_value": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Value of the TXT record to create to prove ownership.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`pending` until the domain is verified, then `verified`.",
			},
		},
	}
}

func (r *domainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *domainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan domainResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	domain := plan.Domain.ValueString()
	body, _ := json.Marshal(map[string]string{"domain": domain})

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostDomainsWithBodyWithResponse(ctx, "application/json", bytes.NewReader(body))

	if err == nil {
		err = northflank.CheckResponse(created)
	}

	if err != nil {
		resp.Diagnostics.AddError("Error registering Northflank domain", fmt.Sprintf("Could not register %q: %s", domain, err))
		return
	}

	tflog.Debug(ctx, "registered Northflank domain", map[string]any{"domain": domain})

	read, err := decodeData[domainRead](created.Body, "domain")

	if err == nil {
		// The create response carries the verification record too, but read
		// back for the canonical form.
		read, err = getDomain(ctx, r.client, domain)
	}

	if err != nil {
		plan.ID = types.StringValue(domain)
		plan.VerificationRecordName = types.StringNull()
		plan.VerificationRecordValue = types.StringNull()
		plan.Status = types.StringNull()
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back registered Northflank domain",
			fmt.Sprintf("%q was registered but could not be read: %s. It has been saved to state; the "+
				"verification record is filled in on the next refresh.", domain, err),
		)
		return
	}

	applyDomain(&plan, read)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *domainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state domainResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	read, err := getDomain(ctx, r.client, state.ID.ValueString())

	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError("Error reading Northflank domain", err.Error())
		return
	}

	applyDomain(&state, read)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never called with a change: every configurable attribute forces
// a replacement. It only refreshes the computed values.
func (r *domainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan domainResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *domainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state domainResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	domain := state.ID.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteDomainsDomainWithResponse(ctx, domain)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(deleted)
	})

	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Error deleting Northflank domain", fmt.Sprintf("Could not delete %q: %s", domain, err))
	}
}

func (r *domainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain"), req.ID)...)
}

func applyDomain(model *domainResourceModel, read *domainRead) {
	model.ID = types.StringValue(read.Name)
	model.Domain = types.StringValue(read.Name)
	model.Status = types.StringValue(read.Status)
	model.VerificationRecordName = types.StringPointerValue(verificationRecordName(read.Hostname, read.Name))
	model.VerificationRecordValue = types.StringPointerValue(read.Token)
}
