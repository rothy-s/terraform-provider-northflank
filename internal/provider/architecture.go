package provider

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var architectures = []string{"arm", "x86"}

// infrastructurePayload is the infrastructure object of service and job
// bodies. Create and update (PUT) take it, but Northflank rejects a change
// after creation ("cannot be modified after creation"), so the attributes
// force a new resource instead.
type infrastructurePayload struct {
	Architecture string `json:"architecture"`
}

// infrastructurePayloadFrom leaves the object out when no architecture is
// configured, so that Northflank applies its default.
func infrastructurePayloadFrom(architecture types.String) *infrastructurePayload {
	if architecture.IsNull() || architecture.IsUnknown() {
		return nil
	}

	return &infrastructurePayload{Architecture: architecture.ValueString()}
}

// serviceArchitectureAttribute is the architecture attribute of services,
// which the API reports back, defaulting to x86.
func serviceArchitectureAttribute(noun string) schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: fmt.Sprintf("CPU architecture of the %[1]s's infrastructure, `arm` or `x86`. For a "+
			"build service this is the architecture of the images it builds; for a deployment it is the "+
			"architecture of the nodes it runs on. Left unset, Northflank uses `x86`. Northflank cannot "+
			"change it, so changing it forces a new %[1]s; removing the attribute keeps the current value.", noun),
		Validators: []validator.String{stringvalidator.OneOf(architectures...)},
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.UseStateForUnknown(),
			stringplanmodifier.RequiresReplace(),
		},
	}
}

// jobArchitectureAttribute is the architecture attribute of jobs. The API
// does not report it for a job, so the provider keeps the configured value
// and cannot detect a change made outside Terraform.
func jobArchitectureAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		MarkdownDescription: "CPU architecture of the nodes the job runs on, `arm` or `x86`. Left unset, " +
			"Northflank uses its default. The API does not report this setting for a job, so a change made " +
			"outside Terraform is not detected. Northflank cannot change it, so changing it forces a new job.",
		Validators: []validator.String{stringvalidator.OneOf(architectures...)},
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.RequiresReplace(),
		},
	}
}

// serviceArchitectureValue reads the architecture a service reports, null
// when it reports none.
func serviceArchitectureValue(service *serviceRead) types.String {
	if service.Infrastructure == nil || service.Infrastructure.Architecture == "" {
		return types.StringNull()
	}

	return types.StringValue(service.Infrastructure.Architecture)
}
