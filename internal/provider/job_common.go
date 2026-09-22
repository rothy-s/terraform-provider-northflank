package provider

import (
	"context"
	"errors"
	"time"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Enumerations and constraints copied from the OpenAPI document.
var (
	jobConcurrencyPolicies = []string{"allow", "forbid", "replace"}

	// Job names follow the same rules as project names, but may be up to 52
	// characters long.
	jobNamePattern = projectNamePattern

	jobPlanPattern = secretGroupIdentifierPattern
)

const (
	jobDefaultConcurrencyPolicy = "forbid"
	jobDefaultBackoffLimit      = 0
)

// jobPayload is the JSON body for both create (POST) and update (PUT), which
// take the same schema. It is hand-written for the same reason as the secret
// group payload, and a test checks it against the generated types.
type jobPayload struct {
	Name               string             `json:"name"`
	Description        *string            `json:"description,omitempty"`
	Tags               []string           `json:"tags"`
	Billing            jobBillingPayload  `json:"billing"`
	Deployment         deploymentPayload  `json:"deployment"`
	Settings           jobSettingsPayload `json:"settings"`
	RuntimeEnvironment map[string]string  `json:"runtimeEnvironment"`
}

type jobBillingPayload struct {
	DeploymentPlan string `json:"deploymentPlan"`
}

type jobSettingsPayload struct {
	BackoffLimit          int             `json:"backoffLimit"`
	ActiveDeadlineSeconds *int            `json:"activeDeadlineSeconds,omitempty"`
	Cron                  *jobCronPayload `json:"cron,omitempty"`
}

type jobCronPayload struct {
	Schedule          string `json:"schedule"`
	ConcurrencyPolicy string `json:"concurrencyPolicy"`
}

// jobView is the part of a job the provider reads back, in one named shape
// for the resource, the data source and the tests.
//
// The API does not report which image or build service a job deploys, nor its
// runtime environment, so those are absent here.
type jobView struct {
	ID                    string
	UID                   string
	AppID                 string
	ProjectID             string
	Name                  string
	Description           *string
	Tags                  []string
	JobType               string
	DeploymentPlan        string
	BackoffLimit          int
	ActiveDeadlineSeconds int
	Cron                  *jobCronPayload
	Command               *string
	Entrypoint            *string
	Suspended             bool
	CreatedAt             time.Time
}

// getJob fetches a job, retrying transient failures.
func getJob(ctx context.Context, client *northflank.ClientWithResponses, projectID, jobID string) (*jobView, error) {
	var view *jobView

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsJobsJobidWithResponse(ctx, projectID, jobID)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		view, err = jobFromGet(got)
		return err
	})

	return view, err
}

func jobFromGet(got *northflank.GetProjectsJobsJobidHTTPResponse) (*jobView, error) {
	if got.JSON200 == nil {
		return nil, errors.New("the API returned a successful status with no job body")
	}

	data := got.JSON200.Data

	view := &jobView{
		ID:                    data.Id,
		UID:                   data.Uid,
		AppID:                 data.AppId,
		ProjectID:             data.ProjectId,
		Name:                  data.Name,
		Description:           data.Description,
		Tags:                  data.Tags,
		JobType:               string(data.JobType),
		DeploymentPlan:        data.Billing.DeploymentPlan,
		BackoffLimit:          data.Settings.BackoffLimit,
		ActiveDeadlineSeconds: data.Settings.ActiveDeadlineSeconds,
		Suspended:             data.Suspended != nil && *data.Suspended,
		CreatedAt:             data.CreatedAt,
	}

	if cron := data.Settings.Cron; cron != nil && cron.Schedule != nil {
		view.Cron = &jobCronPayload{Schedule: *cron.Schedule, ConcurrencyPolicy: jobDefaultConcurrencyPolicy}

		if cron.ConcurrencyPolicy != nil {
			view.Cron.ConcurrencyPolicy = string(*cron.ConcurrencyPolicy)
		}
	}

	if data.Deployment != nil && data.Deployment.Docker != nil {
		docker := data.Deployment.Docker

		switch string(docker.ConfigType) {
		case "customCommand":
			view.Command = docker.CustomCommand
		case "customEntrypoint":
			view.Entrypoint = docker.CustomEntrypoint
		case "customEntrypointCustomCommand":
			view.Command = docker.CustomCommand
			view.Entrypoint = docker.CustomEntrypoint
		}
	}

	return view, nil
}
