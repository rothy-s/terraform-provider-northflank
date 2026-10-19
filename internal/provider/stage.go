package provider

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// stageIDAttribute is the stage_id attribute of resources that can belong to
// one of a project's environments, which the API calls stages.
//
// It is computed as well as optional, and keeps its state while unset, so
// that every update sends the environment the resource is in. Services and
// jobs are updated with PUT, and leaving stageId out of one is not something
// to rely on to keep the resource where it is.
func stageIDAttribute(noun string) schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: fmt.Sprintf("ID of the project environment the %[1]s belongs to. Environments "+
			"are created in the Northflank dashboard. Changing this moves the %[1]s. Left unset, a new %[1]s "+
			"goes in the default environment, and an existing one stays where it is; removing the attribute "+
			"does not move it back.", noun),
		Validators: []validator.String{
			stringvalidator.LengthBetween(3, 100),
			stringvalidator.RegexMatches(secretGroupIdentifierPattern,
				"must contain only letters and digits, with single hyphens between them"),
		},
		PlanModifiers: []planmodifier.String{
			stringplanmodifier.UseStateForUnknown(),
		},
	}
}
