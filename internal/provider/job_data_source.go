package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Ensure the implementation satisfies the framework interfaces.
var (
	_ datasource.DataSource                     = (*jobDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*jobDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*jobDataSource)(nil)
)

type jobDataSource struct {
	client *northflank.ClientWithResponses
}

// NewJobDataSource returns the northflank_job data source implementation.
func NewJobDataSource() datasource.DataSource {
	return &jobDataSource{}
}

// jobDataSourceModel maps the northflank_job data source schema. It covers
// what the API reports about a job; the deployment source and runtime
// environment are not among it.
type jobDataSourceModel struct {
	ID                    types.String `tfsdk:"id"`
	ProjectID             types.String `tfsdk:"project_id"`
	Name                  types.String `tfsdk:"name"`
	UID                   types.String `tfsdk:"uid"`
	AppID                 types.String `tfsdk:"app_id"`
	Description           types.String `tfsdk:"description"`
	Tags                  types.Set    `tfsdk:"tags"`
	JobType               types.String `tfsdk:"job_type"`
	DeploymentPlan        types.String `tfsdk:"deployment_plan"`
	Cron                  types.Object `tfsdk:"cron"`
	BackoffLimit          types.Int64  `tfsdk:"backoff_limit"`
	ActiveDeadlineSeconds types.Int64  `tfsdk:"active_deadline_seconds"`
	Command               types.String `tfsdk:"command"`
	Entrypoint            types.String `tfsdk:"entrypoint"`
	Suspended             types.Bool   `tfsdk:"suspended"`
	CreatedAt             types.String `tfsdk:"created_at"`
}

func (d *jobDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_job"
}

func (d *jobDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a job in a Northflank project, either by its `id` or by its `name`. Set " +
			"exactly one of them.\n\n" +
			"The API does not report which image or build service a job runs, nor its runtime environment, so " +
			"those are not available here.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the project the job belongs to.",
			},
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Identifier of the job. Set this to look the job up by ID.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Name of the job. Set this to look the job up by name, which lists the " +
					"project's jobs to find it.",
			},
			"uid":         computedString("Permanent identifier of the job. Unlike `id`, it never changes and is never reused."),
			"app_id":      computedString("Full identifier Northflank uses for the job's deployment."),
			"description": computedString("Short description of the job."),
			"tags": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Tags attached to the job.",
			},
			"job_type":        computedString("`cron` or `manual`."),
			"deployment_plan": computedString("ID of the compute plan each job run uses."),
			"cron": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The job's schedule. Null for a manual job.",
				Attributes: map[string]schema.Attribute{
					"schedule":           computedString("Cron expression."),
					"concurrency_policy": computedString("`allow`, `forbid` or `replace`."),
				},
			},
			"backoff_limit": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Number of times a failed run is retried.",
			},
			"active_deadline_seconds": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Longest a run may take, in seconds. Null when Northflank's own limit applies.",
			},
			"command":    computedString("Command override. Null when the image's `CMD` is used."),
			"entrypoint": computedString("Entrypoint override. Null when the image's `ENTRYPOINT` is used."),
			"suspended": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether a cron job's schedule is currently suspended.",
			},
			"created_at": computedString("Creation time, in RFC 3339 format."),
		},
	}
}

func (d *jobDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *jobDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, ok := providerDataFrom(req.ProviderData, &resp.Diagnostics)

	if !ok {
		return
	}

	d.client = data.Client
}

func (d *jobDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config jobDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)

	if resp.Diagnostics.HasError() {
		return
	}

	projectID := config.ProjectID.ValueString()
	jobID := config.ID.ValueString()

	if config.ID.IsNull() {
		id, err := d.findJobIDByName(ctx, projectID, config.Name.ValueString())

		if err != nil {
			resp.Diagnostics.AddError("Error looking up Northflank job", err.Error())
			return
		}

		jobID = id
	}

	job, err := getJob(ctx, d.client, projectID, jobID)

	if err != nil {
		summary := "Error reading Northflank job"

		if isNotFound(err) {
			summary = "Northflank job not found"
		}

		resp.Diagnostics.AddError(summary, fmt.Sprintf("Could not read job %q in project %q: %s", jobID, projectID, err))
		return
	}

	model, diags := jobDataSourceModelFrom(ctx, job)
	resp.Diagnostics.Append(diags...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, model)...)
}

// findJobIDByName walks the project's jobs and returns the ID of the one with
// that name.
func (d *jobDataSource) findJobIDByName(ctx context.Context, projectID, name string) (string, error) {
	perPage := projectListPageSize
	params := &northflank.GetProjectsJobsParams{PerPage: &perPage}

	var matches []string

	for {
		var page *northflank.GetProjectsJobsHTTPResponse

		err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
			got, err := d.client.GetProjectsJobsWithResponse(ctx, projectID, params)

			if err != nil {
				return err
			}

			page = got
			return northflank.CheckResponse(got)
		})

		if err != nil {
			return "", fmt.Errorf("could not list jobs in project %q: %w", projectID, err)
		}

		if page.JSON200 == nil {
			return "", fmt.Errorf("could not list jobs in project %q: the API returned a successful status with no body", projectID)
		}

		for _, j := range page.JSON200.Data.Jobs {
			if j.Name == name {
				matches = append(matches, j.Id)
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
		return "", fmt.Errorf("no job named %q exists in project %q", name, projectID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d jobs are named %q in project %q; look the job up by id instead: %v",
			len(matches), name, projectID, matches)
	}
}

func jobDataSourceModelFrom(ctx context.Context, job *jobView) (*jobDataSourceModel, diag.Diagnostics) {
	// Reuse the resource's mapping so both report attributes the same way.
	r := jobResourceModel{Deployment: types.ObjectNull(deploymentSourceAttrTypes)}
	diags := applyJob(ctx, &r, job)

	return &jobDataSourceModel{
		ID:                    r.ID,
		ProjectID:             r.ProjectID,
		Name:                  r.Name,
		UID:                   r.UID,
		AppID:                 r.AppID,
		Description:           r.Description,
		Tags:                  r.Tags,
		JobType:               r.JobType,
		DeploymentPlan:        r.DeploymentPlan,
		Cron:                  r.Cron,
		BackoffLimit:          r.BackoffLimit,
		ActiveDeadlineSeconds: r.ActiveDeadlineSeconds,
		Command:               types.StringPointerValue(job.Command),
		Entrypoint:            types.StringPointerValue(job.Entrypoint),
		Suspended:             r.Suspended,
		CreatedAt:             r.CreatedAt,
	}, diags
}
