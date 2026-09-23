package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/objectvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The deployment block -- what a workload runs -- is shared by jobs and
// deployment services, which describe it with the same API shape. A job can
// also build from Git itself, in which case the source is its repository and
// the block only carries the command and entrypoint overrides.

var (
	// A build service in this project ("my-builder"), or in another one
	// ("other-project/my-builder").
	buildServiceIDPattern = regexp.MustCompile(`^([a-zA-Z0-9]+(-[a-zA-Z0-9]+)*/)?[a-zA-Z](-?[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*)?$`)
	buildSHAPattern       = regexp.MustCompile(`^([a-fA-F0-9]{40}|latest)$`)
)

type deploymentSourceModel struct {
	ExternalImage types.Object `tfsdk:"external_image"`
	BuildService  types.Object `tfsdk:"build_service"`
	Command       types.String `tfsdk:"command"`
	Entrypoint    types.String `tfsdk:"entrypoint"`
}

type externalImageModel struct {
	ImagePath     types.String `tfsdk:"image_path"`
	CredentialsID types.String `tfsdk:"credentials_id"`
}

type buildServiceModel struct {
	ID       types.String `tfsdk:"id"`
	Branch   types.String `tfsdk:"branch"`
	BuildSHA types.String `tfsdk:"build_sha"`
}

var (
	externalImageAttrTypes = map[string]attr.Type{
		"image_path":     types.StringType,
		"credentials_id": types.StringType,
	}

	buildServiceAttrTypes = map[string]attr.Type{
		"id":        types.StringType,
		"branch":    types.StringType,
		"build_sha": types.StringType,
	}

	deploymentSourceAttrTypes = map[string]attr.Type{
		"external_image": types.ObjectType{AttrTypes: externalImageAttrTypes},
		"build_service":  types.ObjectType{AttrTypes: buildServiceAttrTypes},
		"command":        types.StringType,
		"entrypoint":     types.StringType,
	}
)

type deploymentPayload struct {
	External *externalImagePayload `json:"external,omitempty"`
	Internal *buildServicePayload  `json:"internal,omitempty"`
	VCS      *vcsDataPayload       `json:"vcs,omitempty"`
	Docker   *dockerPayload        `json:"docker,omitempty"`
}

type externalImagePayload struct {
	ImagePath   string  `json:"imagePath"`
	Credentials *string `json:"credentials,omitempty"`
}

type buildServicePayload struct {
	ID       string  `json:"id"`
	Branch   *string `json:"branch,omitempty"`
	BuildSHA *string `json:"buildSHA,omitempty"`
}

type dockerPayload struct {
	ConfigType       string  `json:"configType"`
	CustomCommand    *string `json:"customCommand,omitempty"`
	CustomEntrypoint *string `json:"customEntrypoint,omitempty"`
}

// dockerOverride turns separate command and entrypoint overrides into the
// API's configType plus values. Neither set means the image defaults.
func dockerOverride(command, entrypoint *string) *dockerPayload {
	switch {
	case command != nil && entrypoint != nil:
		return &dockerPayload{ConfigType: "customEntrypointCustomCommand", CustomCommand: command, CustomEntrypoint: entrypoint}
	case command != nil:
		return &dockerPayload{ConfigType: "customCommand", CustomCommand: command}
	case entrypoint != nil:
		return &dockerPayload{ConfigType: "customEntrypoint", CustomEntrypoint: entrypoint}
	default:
		return &dockerPayload{ConfigType: "default"}
	}
}

// deploymentPayloadFrom converts a configured deployment block into the
// API's shape: a source, plus the docker override. With requireSource unset,
// the block may be absent or name no source, as for a job that builds from
// Git, whose caller adds the source itself.
func deploymentPayloadFrom(ctx context.Context, value types.Object, requireSource bool) (*deploymentPayload, diag.Diagnostics) {
	var diags diag.Diagnostics
	payload := &deploymentPayload{Docker: dockerOverride(nil, nil)}

	if value.IsNull() || value.IsUnknown() {
		if requireSource {
			diags.AddError("Missing deployment", "The deployment block must be set.")
		}

		return payload, diags
	}

	var deployment deploymentSourceModel
	diags.Append(value.As(ctx, &deployment, objectAsOptions)...)

	if diags.HasError() {
		return nil, diags
	}

	payload.Docker = dockerOverride(knownStringPointer(deployment.Command), knownStringPointer(deployment.Entrypoint))

	switch {
	case !deployment.ExternalImage.IsNull() && !deployment.ExternalImage.IsUnknown():
		var image externalImageModel
		diags.Append(deployment.ExternalImage.As(ctx, &image, objectAsOptions)...)

		payload.External = &externalImagePayload{
			ImagePath:   image.ImagePath.ValueString(),
			Credentials: knownStringPointer(image.CredentialsID),
		}

	case !deployment.BuildService.IsNull() && !deployment.BuildService.IsUnknown():
		var build buildServiceModel
		diags.Append(deployment.BuildService.As(ctx, &build, objectAsOptions)...)

		payload.Internal = &buildServicePayload{
			ID:       build.ID.ValueString(),
			Branch:   knownStringPointer(build.Branch),
			BuildSHA: knownStringPointer(build.BuildSHA),
		}

	case requireSource:
		// Guarded by the schema's ExactlyOneOf, so this is a provider bug.
		diags.AddError("Invalid deployment", "Exactly one of external_image or build_service must be set.")
	}

	return payload, diags
}

// deploymentSourceAttribute is the schema of the deployment block. The
// description differs between resources, since they report the source back
// differently. With sourceOptional, the block itself and its source are
// optional, for a resource that can name its source elsewhere; that resource
// then checks that exactly one source is set.
func deploymentSourceAttribute(description string, sourceOptional bool) schema.SingleNestedAttribute {
	sourceRule := objectvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("build_service"))

	if sourceOptional {
		sourceRule = objectvalidator.ConflictsWith(path.MatchRelative().AtParent().AtName("build_service"))
	}

	return schema.SingleNestedAttribute{
		Required:            !sourceOptional,
		Optional:            sourceOptional,
		MarkdownDescription: description,
		Attributes: map[string]schema.Attribute{
			"external_image": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Run an image from a container registry.",
				Validators: []validator.Object{
					sourceRule,
				},
				Attributes: map[string]schema.Attribute{
					"image_path": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "Image to run, such as `nginx:latest` or " +
							"`ghcr.io/org/image:tag`. Images outside Docker Hub need the registry host.",
						Validators: []validator.String{
							stringvalidator.LengthAtLeast(1),
						},
					},
					"credentials_id": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "ID of saved registry credentials, for a private image.",
					},
				},
			},
			"build_service": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Run the output of a Northflank build service.",
				Attributes: map[string]schema.Attribute{
					"id": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "ID of the build service: `my-builder` in the same project, or " +
							"`other-project/my-builder` in another one.",
						Validators: []validator.String{
							stringvalidator.RegexMatches(buildServiceIDPattern,
								"must be a build service ID, optionally prefixed with its project ID and a slash"),
						},
					},
					"branch": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "Branch whose builds to deploy.",
					},
					"build_sha": schema.StringAttribute{
						Optional: true,
						Computed: true,
						Default:  stringdefault.StaticString("latest"),
						MarkdownDescription: "Commit to deploy: a full 40-character SHA, or `latest` for the " +
							"most recent build. Defaults to `latest`.",
						Validators: []validator.String{
							stringvalidator.RegexMatches(buildSHAPattern, "must be a 40-character commit SHA or latest"),
						},
					},
				},
			},
			"command": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Command to run instead of the image's `CMD`.",
			},
			"entrypoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Entrypoint to run instead of the image's `ENTRYPOINT`.",
			},
		},
	}
}
