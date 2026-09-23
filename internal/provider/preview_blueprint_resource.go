package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ resource.Resource                   = (*previewBlueprintResource)(nil)
	_ resource.ResourceWithConfigure      = (*previewBlueprintResource)(nil)
	_ resource.ResourceWithImportState    = (*previewBlueprintResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*previewBlueprintResource)(nil)
	_ resource.ResourceWithValidateConfig = (*previewBlueprintResource)(nil)
)

type previewBlueprintResource struct {
	client *northflank.ClientWithResponses
}

// NewPreviewBlueprintResource returns the northflank_preview_blueprint
// resource implementation.
func NewPreviewBlueprintResource() resource.Resource {
	return &previewBlueprintResource{}
}

// previewBlueprintModel maps the northflank_preview_blueprint schema.
type previewBlueprintModel struct {
	ID                types.String `tfsdk:"id"`
	ProjectID         types.String `tfsdk:"project_id"`
	Template          types.String `tfsdk:"template"`
	Name              types.String `tfsdk:"name"`
	ArgumentOverrides types.Map    `tfsdk:"argument_overrides"`
	Status            types.String `tfsdk:"status"`
	Paused            types.Bool   `tfsdk:"paused"`
}

// previewBlueprintRead is the part of the get-blueprint response the provider
// uses. The template contents are deliberately not among it.
type previewBlueprintRead struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Paused bool   `json:"paused"`
}

// ---- Schema ----------------------------------------------------------------

func (r *previewBlueprintResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_preview_blueprint"
}

func (r *previewBlueprintResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a preview blueprint: the template Northflank runs to create a preview " +
			"environment, for example for each pull request.\n\n" +
			"The template is Northflank's JSON template format and is passed through as is, usually with " +
			"`file()` from the repository Terraform runs in. Terraform delivers it: the blueprint is never " +
			"linked to the repository through GitOps, so Northflank never writes to it, and a change to the " +
			"file reaches Northflank on the next apply. A `gitops` key in the template is dropped for the " +
			"same reason. Changes made to the template in the Northflank dashboard are not detected.\n\n" +
			"Keep secrets such as webhook trigger tokens out of the template: reference them as " +
			"`${args.NAME}` and set them in `argument_overrides`, which Northflank stores separately.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identifier of the preview blueprint.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the blueprint belongs to. Changing this forces a new blueprint.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"template": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The blueprint as Northflank's JSON template, with at least `name`, " +
					"`apiVersion` and `spec`, such as `file(\"preview-blueprint.json\")`.",
			},
			"name": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Name of the blueprint, taken from the template. A new name forces a new " +
					"blueprint.",
			},
			"argument_overrides": schema.MapAttribute{
				Optional:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "Values for the template's `${args.NAME}` references, stored in Northflank " +
					"apart from the template, such as a webhook trigger token. Stored in Terraform state; not " +
					"read back from the API.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Status of the blueprint's latest run, such as `success` or `running`.",
			},
			"paused": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the blueprint's Git and webhook triggers are paused.",
			},
		},
	}
}

// ValidateConfig checks the template early, so that a malformed file fails at
// plan time.
func (r *previewBlueprintResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var template types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("template"), &template)...)

	if template.IsNull() || template.IsUnknown() {
		return
	}

	if _, _, err := parseBlueprintTemplate(template.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("template"), "Invalid preview blueprint template", err.Error())
	}
}

// ModifyPlan takes the name from the template, and forces a new blueprint when
// it changes: Northflank identifies blueprints by it.
func (r *previewBlueprintResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}

	var template types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("template"), &template)...)

	if template.IsNull() || template.IsUnknown() {
		return
	}

	_, name, err := parseBlueprintTemplate(template.ValueString())

	if err != nil {
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("name"), name)...)

	if req.State.Raw.IsNull() {
		return
	}

	var prior types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("name"), &prior)...)

	if !prior.IsNull() && prior.ValueString() != name {
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("name"))
	}
}

func (r *previewBlueprintResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	r.client = data.Client
}

// ---- CRUD ------------------------------------------------------------------

func (r *previewBlueprintResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan previewBlueprintModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, name, diags := previewBlueprintBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := plan.ProjectID.ValueString()

	// Creates are not retried: a failure does not prove nothing was made.
	created, err := r.client.PostProjectsPreviewBlueprintsWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))

	if err == nil {
		err = northflank.CheckResponse(created)
	}

	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating Northflank preview blueprint",
			fmt.Sprintf("Could not create preview blueprint %q in project %q: %s", name, projectID, err),
		)
		return
	}

	// The create response is not documented to carry the new ID, so fall
	// back to finding the blueprint by name.
	blueprintID := idFromBody(created.Body)

	if blueprintID == "" {
		blueprintID, err = findPreviewBlueprintID(ctx, r.client, projectID, name)

		if err != nil {
			resp.Diagnostics.AddError(
				"Error finding created Northflank preview blueprint",
				fmt.Sprintf("Preview blueprint %q was created but its ID could not be found: %s. Import it to "+
					"bring it under Terraform.", name, err),
			)
			return
		}
	}

	tflog.Debug(ctx, "created Northflank preview blueprint", map[string]any{"project_id": projectID, "id": blueprintID})

	plan.ID = types.StringValue(blueprintID)
	plan.Name = types.StringValue(name)
	plan.Status = types.StringNull()
	plan.Paused = types.BoolValue(false)

	read, err := getPreviewBlueprint(ctx, r.client, projectID, blueprintID)

	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"Error reading back created Northflank preview blueprint",
			fmt.Sprintf("Preview blueprint %q was created with ID %q but could not be read: %s. It has been "+
				"saved to state.", name, blueprintID, err),
		)
		return
	}

	applyPreviewBlueprint(&plan, read)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *previewBlueprintResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state previewBlueprintModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	read, err := getPreviewBlueprint(ctx, r.client, state.ProjectID.ValueString(), state.ID.ValueString())

	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}

		resp.Diagnostics.AddError("Error reading Northflank preview blueprint", err.Error())
		return
	}

	applyPreviewBlueprint(&state, read)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *previewBlueprintResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state previewBlueprintModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	body, _, diags := previewBlueprintBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, blueprintID := state.ProjectID.ValueString(), state.ID.ValueString()

	// The whole blueprint is sent, so the update is idempotent.
	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		updated, err := r.client.PostProjectsPreviewBlueprintsPreviewblueprintidWithBodyWithResponse(
			ctx, projectID, blueprintID, "application/json", bytes.NewReader(body))

		if err != nil {
			return err
		}

		return northflank.CheckResponse(updated)
	})

	if err != nil {
		resp.Diagnostics.AddError(
			"Error updating Northflank preview blueprint",
			fmt.Sprintf("Could not update preview blueprint %q: %s", blueprintID, err),
		)
		return
	}

	plan.ID = state.ID

	read, err := getPreviewBlueprint(ctx, r.client, projectID, blueprintID)

	if err != nil {
		resp.Diagnostics.AddError("Error reading Northflank preview blueprint", err.Error())
		return
	}

	applyPreviewBlueprint(&plan, read)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *previewBlueprintResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state previewBlueprintModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID, blueprintID := state.ProjectID.ValueString(), state.ID.ValueString()

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := r.client.DeleteProjectsPreviewBlueprintsPreviewblueprintidWithResponse(ctx, projectID, blueprintID)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(deleted)
	})

	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError(
			"Error deleting Northflank preview blueprint",
			fmt.Sprintf("Could not delete preview blueprint %q: %s", blueprintID, err),
		)
	}
}

func (r *previewBlueprintResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, blueprintID, ok := strings.Cut(req.ID, "/")

	if !ok || projectID == "" || blueprintID == "" || strings.Contains(blueprintID, "/") {
		resp.Diagnostics.AddError(
			"Invalid import identifier",
			fmt.Sprintf("Expected <project_id>/<preview_blueprint_id>, got %q.", req.ID),
		)
		return
	}

	// The template and argument overrides come from configuration on the
	// next apply, which sends them.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), blueprintID)...)
}

// ---- Helpers ---------------------------------------------------------------

// parseBlueprintTemplate decodes a template and checks the fields the API
// requires, returning the template and its name.
func parseBlueprintTemplate(raw string) (map[string]any, string, error) {
	var template map[string]any

	if err := json.Unmarshal([]byte(raw), &template); err != nil {
		return nil, "", fmt.Errorf("the template is not a JSON object: %w", err)
	}

	for _, key := range []string{"name", "apiVersion", "spec"} {
		if _, ok := template[key]; !ok {
			return nil, "", fmt.Errorf("the template has no %q", key)
		}
	}

	name, ok := template["name"].(string)

	if !ok || name == "" {
		return nil, "", errors.New(`the template's "name" must be a non-empty string`)
	}

	if _, ok := template["spec"].(map[string]any); !ok {
		return nil, "", errors.New(`the template's "spec" must be an object`)
	}

	return template, name, nil
}

// previewBlueprintBody builds the request from the template, with argument
// overrides from configuration taking precedence over any the template
// carries.
func previewBlueprintBody(ctx context.Context, plan *previewBlueprintModel) ([]byte, string, diag.Diagnostics) {
	var diags diag.Diagnostics

	template, name, err := parseBlueprintTemplate(plan.Template.ValueString())

	if err != nil {
		diags.AddAttributeError(path.Root("template"), "Invalid preview blueprint template", err.Error())
		return nil, "", diags
	}

	// Never link the blueprint to a repository, even if the file asks to:
	// with GitOps, Northflank writes the template back to the repository.
	delete(template, "gitops")

	if !plan.ArgumentOverrides.IsNull() && !plan.ArgumentOverrides.IsUnknown() {
		overrides := map[string]string{}
		diags.Append(plan.ArgumentOverrides.ElementsAs(ctx, &overrides, false)...)
		template["argumentOverrides"] = overrides
	}

	body, err := json.Marshal(template)

	if err != nil {
		diags.AddError("Error building Northflank preview blueprint request", err.Error())
	}

	return body, name, diags
}

func getPreviewBlueprint(ctx context.Context, client *northflank.ClientWithResponses, projectID, blueprintID string) (*previewBlueprintRead, error) {
	var read *previewBlueprintRead

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsPreviewBlueprintsPreviewblueprintidWithResponse(ctx, projectID, blueprintID)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		read, err = decodeData[previewBlueprintRead](got.Body, "preview blueprint")
		return err
	})

	return read, err
}

// findPreviewBlueprintID walks the project's blueprints for the one with
// that name.
func findPreviewBlueprintID(ctx context.Context, client *northflank.ClientWithResponses, projectID, name string) (string, error) {
	perPage := projectListPageSize
	params := &northflank.GetProjectsPreviewBlueprintsParams{PerPage: &perPage}

	var matches []string

	for {
		var page *northflank.GetProjectsPreviewBlueprintsHTTPResponse

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			got, err := client.GetProjectsPreviewBlueprintsWithResponse(ctx, projectID, params)

			if err != nil {
				return err
			}

			page = got
			return northflank.CheckResponse(got)
		})

		if err != nil {
			return "", fmt.Errorf("could not list preview blueprints in project %q: %w", projectID, err)
		}

		if page.JSON200 == nil {
			return "", fmt.Errorf("could not list preview blueprints in project %q: the API returned a successful status with no body", projectID)
		}

		for _, b := range page.JSON200.Data.Workflows {
			if b.Name == name {
				matches = append(matches, b.Id)
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
		return "", fmt.Errorf("no preview blueprint named %q exists in project %q", name, projectID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d preview blueprints are named %q in project %q: %v", len(matches), name, projectID, matches)
	}
}

// idFromBody returns data.id from a response body, if it carries one.
func idFromBody(body []byte) string {
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}

	return envelope.Data.ID
}

// applyPreviewBlueprint copies what the API reports onto the model. The
// template and argument overrides keep their configured values.
func applyPreviewBlueprint(model *previewBlueprintModel, read *previewBlueprintRead) {
	model.Name = types.StringValue(read.Name)
	model.Status = types.StringValue(read.Status)
	model.Paused = types.BoolValue(read.Paused)
}
