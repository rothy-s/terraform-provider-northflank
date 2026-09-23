package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
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
	_ resource.Resource              = (*verificationResource)(nil)
	_ resource.ResourceWithConfigure = (*verificationResource)(nil)
)

// verificationResource waits for Northflank to verify a domain or a
// subdomain. It stands apart from the domain and subdomain resources for the
// same reason aws_acm_certificate_validation stands apart from the
// certificate: the DNS record it depends on is created in between, often by
// another provider in the same apply.
type verificationResource struct {
	client *northflank.ClientWithResponses

	// subdomain is set for northflank_subdomain_verification.
	subdomain bool
}

// NewDomainVerificationResource returns the northflank_domain_verification
// resource implementation.
func NewDomainVerificationResource() resource.Resource {
	return &verificationResource{}
}

// NewSubdomainVerificationResource returns the
// northflank_subdomain_verification resource implementation.
func NewSubdomainVerificationResource() resource.Resource {
	return &verificationResource{subdomain: true}
}

// verificationModel maps both verification schemas; Subdomain and FullName
// are only part of the subdomain one.
type verificationModel struct {
	ID        types.String `tfsdk:"id"`
	Domain    types.String `tfsdk:"domain"`
	Subdomain types.String `tfsdk:"subdomain"`
	FullName  types.String `tfsdk:"full_name"`
	Timeout   types.String `tfsdk:"timeout"`
}

type domainVerificationModel struct {
	ID      types.String `tfsdk:"id"`
	Domain  types.String `tfsdk:"domain"`
	Timeout types.String `tfsdk:"timeout"`
}

func (r *verificationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	if r.subdomain {
		resp.TypeName = req.ProviderTypeName + "_subdomain_verification"
	} else {
		resp.TypeName = req.ProviderTypeName + "_domain_verification"
	}
}

func (r *verificationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}

	attributes := map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The verified domain, or `<domain>/<subdomain>`.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
		"domain": schema.StringAttribute{
			Required:            true,
			MarkdownDescription: "Root domain, as registered with `northflank_domain`.",
			Validators: []validator.String{
				stringvalidator.RegexMatches(domainNamePattern, "must be a domain name such as example.com"),
			},
			PlanModifiers: replace,
		},
		"timeout": schema.StringAttribute{
			Optional: true,
			MarkdownDescription: "How long to keep retrying while the DNS record propagates, as a duration " +
				"such as `30m`. Defaults to `20m`.",
		},
	}

	if !r.subdomain {
		resp.Schema = schema.Schema{
			MarkdownDescription: "Waits until Northflank has verified ownership of a domain registered with " +
				"`northflank_domain`, retrying while the TXT record propagates. Make it depend on the DNS " +
				"record, and use its `domain` for `northflank_subdomain`, which needs a verified domain.\n\n" +
				"Destroying this resource does not unverify anything; it only forgets the verification.",
			Attributes: attributes,
		}

		return
	}

	attributes["subdomain"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "Subdomain, as added with `northflank_subdomain`, such as `api`.",
		Validators: []validator.String{
			stringvalidator.RegexMatches(subdomainPattern, "must be a lowercase subdomain, @ or *"),
		},
		PlanModifiers: replace,
	}
	attributes["full_name"] = schema.StringAttribute{
		Computed: true,
		MarkdownDescription: "Full name of the verified subdomain, such as `api.example.com`. List it in a " +
			"service's `ports[].domains` to route it; referencing it from there makes the service wait for " +
			"verification.",
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.UseStateForUnknown(),
		},
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Waits until Northflank has verified a subdomain added with `northflank_subdomain`, " +
			"retrying while its CNAME record propagates. Make it depend on the DNS record.\n\n" +
			"Destroying this resource does not unverify anything; it only forgets the verification.",
		Attributes: attributes,
	}
}

func (r *verificationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

func (r *verificationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	model, ok := r.get(ctx, req.Plan.Get, &resp.Diagnostics)

	if !ok {
		return
	}

	timeout, err := parseVerificationTimeout(model.Timeout.ValueString())

	if err != nil {
		resp.Diagnostics.AddError("Invalid timeout", err.Error())
		return
	}

	domain, subdomain := model.Domain.ValueString(), model.Subdomain.ValueString()
	what := fmt.Sprintf("domain %q", domain)

	if r.subdomain {
		what = fmt.Sprintf("subdomain %q of %q", subdomain, domain)
	}

	tflog.Debug(ctx, "verifying "+what)

	err = verifyUntil(ctx, timeout, what, func(ctx context.Context) error {
		return apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			var (
				resp apierr.Response
				err  error
			)

			if r.subdomain {
				resp, err = r.client.PostDomainsDomainSubdomainsSubdomainVerifyWithResponse(ctx, domain, subdomain)
			} else {
				resp, err = r.client.PostDomainsDomainVerifyWithResponse(ctx, domain)
			}

			if err != nil {
				return err
			}

			return northflank.CheckResponse(resp)
		})
	})

	if err != nil {
		resp.Diagnostics.AddError("Northflank verification failed", err.Error())
		return
	}

	model.ID = types.StringValue(domain)

	if r.subdomain {
		model.ID = types.StringValue(domain + "/" + subdomain)
		model.FullName = types.StringNull()

		if read, err := getSubdomain(ctx, r.client, domain, subdomain); err == nil {
			model.FullName = types.StringValue(read.FullName)
		}
	}

	r.set(ctx, model, resp.State.Set, &resp.Diagnostics)
}

// Read drops the resource when the domain or subdomain is no longer verified
// or no longer exists, so that the next apply verifies it again.
func (r *verificationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	model, ok := r.get(ctx, req.State.Get, &resp.Diagnostics)

	if !ok {
		return
	}

	domain, subdomain := model.Domain.ValueString(), model.Subdomain.ValueString()

	var (
		verified bool
		err      error
	)

	if r.subdomain {
		var read *subdomainRead

		if read, err = getSubdomain(ctx, r.client, domain, subdomain); err == nil {
			verified = read.Verified
			model.FullName = types.StringValue(read.FullName)
		}
	} else {
		var read *domainRead

		if read, err = getDomain(ctx, r.client, domain); err == nil {
			verified = read.Status == "verified"
		}
	}

	if isNotFound(err) || (err == nil && !verified) {
		tflog.Debug(ctx, "Northflank verification no longer holds, removing from state", map[string]any{"id": model.ID.ValueString()})
		resp.State.RemoveResource(ctx)
		return
	}

	if err != nil {
		resp.Diagnostics.AddError("Error reading Northflank verification", err.Error())
		return
	}

	r.set(ctx, model, resp.State.Set, &resp.Diagnostics)
}

// Update only happens for a new timeout, which has no effect once verified.
func (r *verificationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	model, ok := r.get(ctx, req.Plan.Get, &resp.Diagnostics)

	if !ok {
		return
	}

	var prior verificationModel

	if state, ok := r.get(ctx, req.State.Get, &resp.Diagnostics); ok {
		prior = *state
	}

	model.ID, model.FullName = prior.ID, prior.FullName
	r.set(ctx, model, resp.State.Set, &resp.Diagnostics)
}

// Delete forgets the verification. There is nothing to undo in Northflank.
func (r *verificationResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

// get and set translate between the two schemas and the shared model, since
// the domain schema lacks the subdomain attributes.
func (r *verificationResource) get(ctx context.Context, get func(context.Context, any) diag.Diagnostics, diags *diag.Diagnostics) (*verificationModel, bool) {
	var model verificationModel

	if r.subdomain {
		diags.Append(get(ctx, &model)...)
		return &model, !diags.HasError()
	}

	var domain domainVerificationModel
	diags.Append(get(ctx, &domain)...)

	model = verificationModel{ID: domain.ID, Domain: domain.Domain, Timeout: domain.Timeout}
	return &model, !diags.HasError()
}

func (r *verificationResource) set(ctx context.Context, model *verificationModel, set func(context.Context, any) diag.Diagnostics, diags *diag.Diagnostics) {
	if r.subdomain {
		diags.Append(set(ctx, model)...)
		return
	}

	diags.Append(set(ctx, &domainVerificationModel{ID: model.ID, Domain: model.Domain, Timeout: model.Timeout})...)
}
