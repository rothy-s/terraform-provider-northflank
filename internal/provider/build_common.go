package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/objectvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The repository and build blocks -- where code comes from and how it is
// built -- are shared by build services and combined services, which describe
// them with the same API shape.

// Enumerations and constraints copied from the OpenAPI document.
var (
	vcsProviders   = []string{"github", "gitlab", "bitbucket", "azure", "self-hosted", "origin"}
	buildEngines   = []string{"buildkit", "kaniko"}
	buildpackStack = []string{
		"HEROKU_24", "HEROKU_22", "HEROKU_22_CLASSIC", "HEROKU_20", "HEROKU_18", "GOOGLE_22", "GOOGLE_V1",
		"CNB_ALPINE", "CNB_BIONIC", "PAKETO_JAMMY_TINY", "PAKETO_JAMMY_BASE", "PAKETO_JAMMY_FULL",
	}

	// Absolute paths without "." or ".." segments.
	buildFilePathPattern = regexp.MustCompile(`^(/[a-zA-Z0-9._-]+)+$`)
	buildDirPattern      = regexp.MustCompile(`^/([a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)*)?$`)
	// A branch rule may use * as a wildcard, and may not contain the
	// characters Git forbids in ref names.
	branchRulePattern = regexp.MustCompile(`^[^?:@$~ \[\]{}]*$`)
)

const (
	defaultDockerfilePath   = "/Dockerfile"
	defaultDockerWorkDir    = "/"
	defaultBuildEngine      = "buildkit"
	defaultBuildpackBuilder = "HEROKU_24"
	defaultBuildContext     = "/"
)

// ---- Terraform models ------------------------------------------------------

type repositoryModel struct {
	URL             types.String `tfsdk:"url"`
	Provider        types.String `tfsdk:"provider"`
	AccountLogin    types.String `tfsdk:"account_login"`
	VcsLinkID       types.String `tfsdk:"vcs_link_id"`
	SelfHostedVcsID types.String `tfsdk:"self_hosted_vcs_id"`
}

type buildSettingsModel struct {
	Dockerfile types.Object `tfsdk:"dockerfile"`
	Buildpack  types.Object `tfsdk:"buildpack"`
}

type dockerfileBuildModel struct {
	Path        types.String `tfsdk:"path"`
	WorkDir     types.String `tfsdk:"work_dir"`
	Target      types.String `tfsdk:"target"`
	BuildEngine types.String `tfsdk:"build_engine"`
	UseCache    types.Bool   `tfsdk:"use_cache"`
}

type buildpackBuildModel struct {
	Builder      types.String `tfsdk:"builder"`
	BuildContext types.String `tfsdk:"build_context"`
	Buildpacks   types.List   `tfsdk:"buildpacks"`
	UseCache     types.Bool   `tfsdk:"use_cache"`
}

var (
	repositoryAttrTypes = map[string]attr.Type{
		"url":                types.StringType,
		"provider":           types.StringType,
		"account_login":      types.StringType,
		"vcs_link_id":        types.StringType,
		"self_hosted_vcs_id": types.StringType,
	}

	dockerfileBuildAttrTypes = map[string]attr.Type{
		"path":         types.StringType,
		"work_dir":     types.StringType,
		"target":       types.StringType,
		"build_engine": types.StringType,
		"use_cache":    types.BoolType,
	}

	buildpackBuildAttrTypes = map[string]attr.Type{
		"builder":       types.StringType,
		"build_context": types.StringType,
		"buildpacks":    types.ListType{ElemType: types.StringType},
		"use_cache":     types.BoolType,
	}

	buildSettingsAttrTypes = map[string]attr.Type{
		"dockerfile": types.ObjectType{AttrTypes: dockerfileBuildAttrTypes},
		"buildpack":  types.ObjectType{AttrTypes: buildpackBuildAttrTypes},
	}
)

// ---- API payloads ----------------------------------------------------------

type vcsDataPayload struct {
	ProjectURL      string  `json:"projectUrl"`
	ProjectType     string  `json:"projectType"`
	ProjectBranch   *string `json:"projectBranch,omitempty"`
	AccountLogin    *string `json:"accountLogin,omitempty"`
	VcsLinkID       *string `json:"vcsLinkId,omitempty"`
	SelfHostedVcsID *string `json:"selfHostedVcsId,omitempty"`
}

type buildSettingsPayload struct {
	Dockerfile *dockerfileSettingsPayload `json:"dockerfile,omitempty"`
	Buildpack  *buildpackSettingsPayload  `json:"buildpack,omitempty"`
}

type dockerfileSettingsPayload struct {
	BuildEngine    string                   `json:"buildEngine"`
	DockerFilePath string                   `json:"dockerFilePath"`
	DockerWorkDir  string                   `json:"dockerWorkDir"`
	Buildkit       *buildkitSettingsPayload `json:"buildkit,omitempty"`
}

type buildkitSettingsPayload struct {
	UseCache bool `json:"useCache"`
}

type buildpackSettingsPayload struct {
	Builder           string   `json:"builder"`
	BuildContext      string   `json:"buildContext"`
	BuildpackLocators []string `json:"buildpackLocators"`
	UseCache          bool     `json:"useCache"`
}

// buildConfigurationPayload is the subset of buildConfiguration the provider
// manages. The branch and pull request rules exist only on build services.
type buildConfigurationPayload struct {
	DockerfileTarget   *string   `json:"dockerfileTarget,omitempty"`
	BranchRestrictions *[]string `json:"branchRestrictions,omitempty"`
	PrRestrictions     *[]string `json:"prRestrictions,omitempty"`
}

// ---- Schema ----------------------------------------------------------------

// repositoryAttribute is the schema of the repository block.
func repositoryAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Required: true,
		MarkdownDescription: "Git repository to build from. Northflank reaches it through a Git account linked " +
			"to the team in the Northflank dashboard, which this provider does not manage.",
		Attributes: map[string]schema.Attribute{
			"url": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "URL of the repository, such as `https://github.com/acme/api`.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Git provider hosting the repository: `github`, `gitlab`, `bitbucket`, " +
					"`azure`, `self-hosted` or `origin`.",
				Validators: []validator.String{
					stringvalidator.OneOf(vcsProviders...),
				},
			},
			"account_login": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Login of the linked Git account to use, when several accounts of the same " +
					"provider are linked. Northflank picks one with access otherwise.",
			},
			"vcs_link_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "ID of the linked Git account to use, as an alternative to `account_login`.",
				Validators: []validator.String{
					stringvalidator.LengthAtMost(24),
				},
			},
			"self_hosted_vcs_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "ID of the self-hosted Git server, when `provider` is `self-hosted`.",
			},
		},
	}
}

// buildSettingsAttribute is the schema of the build block.
func buildSettingsAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Required:            true,
		MarkdownDescription: "How the code is built. Set exactly one of `dockerfile` or `buildpack`.",
		Attributes: map[string]schema.Attribute{
			"dockerfile": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Build with a Dockerfile from the repository.",
				Validators: []validator.Object{
					objectvalidator.ExactlyOneOf(path.MatchRelative().AtParent().AtName("buildpack")),
				},
				Attributes: map[string]schema.Attribute{
					"path": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						Default:             stringdefault.StaticString(defaultDockerfilePath),
						MarkdownDescription: "Path of the Dockerfile in the repository. Defaults to `" + defaultDockerfilePath + "`.",
						Validators: []validator.String{
							stringvalidator.RegexMatches(buildFilePathPattern, "must be an absolute path such as /Dockerfile"),
						},
					},
					"work_dir": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						Default:             stringdefault.StaticString(defaultDockerWorkDir),
						MarkdownDescription: "Build context directory in the repository. Defaults to `" + defaultDockerWorkDir + "`.",
						Validators: []validator.String{
							stringvalidator.RegexMatches(buildDirPattern, "must be an absolute directory such as / or /app"),
						},
					},
					"target": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "Stage to build, for a multi-stage Dockerfile.",
					},
					"build_engine": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						Default:             stringdefault.StaticString(defaultBuildEngine),
						MarkdownDescription: "`buildkit` or `kaniko`. Defaults to `" + defaultBuildEngine + "`.",
						Validators: []validator.String{
							stringvalidator.OneOf(buildEngines...),
						},
					},
					"use_cache": schema.BoolAttribute{
						Optional: true,
						Computed: true,
						Default:  booldefault.StaticBool(false),
						MarkdownDescription: "Keep build layers in persistent storage between builds. Only applies " +
							"to the `buildkit` engine. Defaults to `false`.",
					},
				},
			},
			"buildpack": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Build with Cloud Native Buildpacks, without a Dockerfile.",
				Attributes: map[string]schema.Attribute{
					"builder": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						Default:             stringdefault.StaticString(defaultBuildpackBuilder),
						MarkdownDescription: "Buildpack stack. Defaults to `" + defaultBuildpackBuilder + "`.",
						Validators: []validator.String{
							stringvalidator.OneOf(buildpackStack...),
						},
					},
					"build_context": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						Default:             stringdefault.StaticString(defaultBuildContext),
						MarkdownDescription: "Directory in the repository to build. Defaults to `" + defaultBuildContext + "`.",
						Validators: []validator.String{
							stringvalidator.RegexMatches(buildDirPattern, "must be an absolute directory such as / or /app"),
						},
					},
					"buildpacks": schema.ListAttribute{
						Optional:            true,
						Computed:            true,
						ElementType:         types.StringType,
						Default:             listdefault.StaticValue(types.ListValueMust(types.StringType, nil)),
						MarkdownDescription: "Custom buildpacks to use instead of the stack's defaults, in order.",
					},
					"use_cache": schema.BoolAttribute{
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
						MarkdownDescription: "Cache build dependencies between builds. Defaults to `false`.",
					},
				},
			},
		},
	}
}

// branchRuleListAttribute is the schema of branch_restrictions and
// pr_restrictions on build services.
func branchRuleListAttribute(description string) schema.ListAttribute {
	return schema.ListAttribute{
		Optional:            true,
		Computed:            true,
		ElementType:         types.StringType,
		Default:             listdefault.StaticValue(types.ListValueMust(types.StringType, nil)),
		MarkdownDescription: description,
		Validators: []validator.List{
			listvalidator.ValueStringsAre(
				stringvalidator.LengthAtLeast(1),
				stringvalidator.RegexMatches(branchRulePattern, "must be a branch name, optionally with * wildcards"),
			),
		},
	}
}

// ---- Conversions -----------------------------------------------------------

// vcsDataPayloadFrom converts the repository block. branch is only set for
// combined services, which deploy one branch.
func vcsDataPayloadFrom(ctx context.Context, value types.Object, branch *string) (*vcsDataPayload, diag.Diagnostics) {
	var repo repositoryModel
	diags := value.As(ctx, &repo, objectAsOptions)

	return &vcsDataPayload{
		ProjectURL:      repo.URL.ValueString(),
		ProjectType:     repo.Provider.ValueString(),
		ProjectBranch:   branch,
		AccountLogin:    knownStringPointer(repo.AccountLogin),
		VcsLinkID:       knownStringPointer(repo.VcsLinkID),
		SelfHostedVcsID: knownStringPointer(repo.SelfHostedVcsID),
	}, diags
}

// buildSettingsPayloadFrom converts the build block, returning the Dockerfile
// target separately since the API keeps it in buildConfiguration.
func buildSettingsPayloadFrom(ctx context.Context, value types.Object) (*buildSettingsPayload, *string, diag.Diagnostics) {
	var settings buildSettingsModel
	diags := value.As(ctx, &settings, objectAsOptions)

	if diags.HasError() {
		return nil, nil, diags
	}

	payload := &buildSettingsPayload{}

	switch {
	case !settings.Dockerfile.IsNull() && !settings.Dockerfile.IsUnknown():
		var docker dockerfileBuildModel
		diags.Append(settings.Dockerfile.As(ctx, &docker, objectAsOptions)...)

		payload.Dockerfile = &dockerfileSettingsPayload{
			BuildEngine:    docker.BuildEngine.ValueString(),
			DockerFilePath: docker.Path.ValueString(),
			DockerWorkDir:  docker.WorkDir.ValueString(),
		}

		// The cache setting belongs to the buildkit engine only.
		if payload.Dockerfile.BuildEngine == "buildkit" {
			payload.Dockerfile.Buildkit = &buildkitSettingsPayload{UseCache: docker.UseCache.ValueBool()}
		}

		return payload, knownStringPointer(docker.Target), diags

	case !settings.Buildpack.IsNull() && !settings.Buildpack.IsUnknown():
		var pack buildpackBuildModel
		diags.Append(settings.Buildpack.As(ctx, &pack, objectAsOptions)...)

		locators := []string{}

		if !pack.Buildpacks.IsNull() && !pack.Buildpacks.IsUnknown() {
			diags.Append(pack.Buildpacks.ElementsAs(ctx, &locators, false)...)
		}

		payload.Buildpack = &buildpackSettingsPayload{
			Builder:           pack.Builder.ValueString(),
			BuildContext:      pack.BuildContext.ValueString(),
			BuildpackLocators: locators,
			UseCache:          pack.UseCache.ValueBool(),
		}

		return payload, nil, diags
	}

	// Guarded by the schema's ExactlyOneOf, so this is a provider bug.
	diags.AddError("Invalid build settings", "Exactly one of dockerfile or buildpack must be set.")
	return nil, nil, diags
}

// repositoryValue converts the reported repository into the block.
//
// Northflank may report the Git account it picked even when the configuration
// left the choice to it. The optional account fields therefore stay unset
// when the prior value is unset, so that Terraform does not see an
// inconsistent result; with no prior value, as after an import, they are
// recorded as reported.
func repositoryValue(ctx context.Context, prior types.Object, service *serviceRead) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics

	if service.VcsData == nil {
		return types.ObjectNull(repositoryAttrTypes), nil
	}

	v := service.VcsData
	accountLogin := types.StringPointerValue(v.AccountLogin)
	vcsLinkID := types.StringPointerValue(v.VcsLinkID)
	selfHostedVcsID := types.StringPointerValue(v.SelfHostedVcsID)

	if !prior.IsNull() && !prior.IsUnknown() {
		var repo repositoryModel
		diags.Append(prior.As(ctx, &repo, objectAsOptions)...)

		if repo.AccountLogin.IsNull() {
			accountLogin = types.StringNull()
		}

		if repo.VcsLinkID.IsNull() {
			vcsLinkID = types.StringNull()
		}

		if repo.SelfHostedVcsID.IsNull() {
			selfHostedVcsID = types.StringNull()
		}
	}

	return types.ObjectValueMust(repositoryAttrTypes, map[string]attr.Value{
		"url":                types.StringValue(v.ProjectURL),
		"provider":           types.StringValue(v.ProjectType),
		"account_login":      accountLogin,
		"vcs_link_id":        vcsLinkID,
		"self_hosted_vcs_id": selfHostedVcsID,
	}), diags
}

// buildSettingsValue converts the reported build settings into the block.
func buildSettingsValue(ctx context.Context, service *serviceRead) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics

	dockerfile := types.ObjectNull(dockerfileBuildAttrTypes)
	buildpack := types.ObjectNull(buildpackBuildAttrTypes)

	if s := service.BuildSettings; s != nil && s.Dockerfile != nil {
		d := s.Dockerfile
		engine := defaultBuildEngine

		if d.BuildEngine != "" {
			engine = d.BuildEngine
		}

		var target *string

		if service.BuildConfiguration != nil {
			target = service.BuildConfiguration.DockerfileTarget
		}

		dockerfile = types.ObjectValueMust(dockerfileBuildAttrTypes, map[string]attr.Value{
			"path":         types.StringValue(orDefault(d.DockerFilePath, defaultDockerfilePath)),
			"work_dir":     types.StringValue(orDefault(d.DockerWorkDir, defaultDockerWorkDir)),
			"target":       types.StringPointerValue(target),
			"build_engine": types.StringValue(engine),
			"use_cache":    types.BoolValue(d.Buildkit != nil && d.Buildkit.UseCache),
		})
	}

	if s := service.BuildSettings; s != nil && s.Buildpack != nil {
		b := s.Buildpack

		locators, d := types.ListValueFrom(ctx, types.StringType, nonNil(b.BuildpackLocators))
		diags.Append(d...)

		buildpack = types.ObjectValueMust(buildpackBuildAttrTypes, map[string]attr.Value{
			"builder":       types.StringValue(orDefault(b.Builder, defaultBuildpackBuilder)),
			"build_context": types.StringValue(orDefault(b.BuildContext, defaultBuildContext)),
			"buildpacks":    locators,
			"use_cache":     types.BoolValue(b.UseCache),
		})
	}

	return types.ObjectValueMust(buildSettingsAttrTypes, map[string]attr.Value{
		"dockerfile": dockerfile,
		"buildpack":  buildpack,
	}), diags
}

// stringMapOrNull converts a reported map, with no entries meaning null.
func stringMapOrNull(ctx context.Context, values map[string]string, diags *diag.Diagnostics) types.Map {
	if len(values) == 0 {
		return types.MapNull(types.StringType)
	}

	value, d := types.MapValueFrom(ctx, types.StringType, values)
	diags.Append(d...)

	return value
}

// stringsFromList returns the elements of a known list, or an empty slice.
func stringsFromList(ctx context.Context, list types.List, diags *diag.Diagnostics) []string {
	out := []string{}

	if list.IsNull() || list.IsUnknown() {
		return out
	}

	diags.Append(list.ElementsAs(ctx, &out, false)...)
	return out
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
