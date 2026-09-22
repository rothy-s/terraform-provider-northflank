package provider

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Retry policy for idempotent calls. Northflank rate-limits per token, and
// Terraform runs independent operations in parallel.
const (
	retryAttempts  = 4
	retryBaseDelay = 2 * time.Second
)

// Constraints copied from the OpenAPI document, so that a bad value fails at
// plan time rather than halfway through an apply.
var (
	projectNamePattern        = regexp.MustCompile(`^[a-zA-Z]((-|\s)?[a-zA-Z0-9]+((-|\s)[a-zA-Z0-9]+)*)?$`)
	projectDescriptionPattern = regexp.MustCompile("^[a-zA-Z0-9.,?\\s\\\\/'\"()[\\];`%^&*\\-_:!]+$")
	projectColorPattern       = regexp.MustCompile(`^#([a-fA-F0-9]{6}|[a-fA-F0-9]{3})$`)
)

func projectNameValidators() []validator.String {
	return []validator.String{
		stringvalidator.LengthBetween(3, 39),
		stringvalidator.RegexMatches(projectNamePattern,
			"must start with a letter and contain only letters, digits, and single spaces or hyphens between them"),
	}
}

func projectDescriptionValidators() []validator.String {
	return []validator.String{
		stringvalidator.LengthBetween(1, 200),
		stringvalidator.RegexMatches(projectDescriptionPattern, "contains characters Northflank does not accept"),
	}
}

func projectColorValidators() []validator.String {
	return []validator.String{
		stringvalidator.RegexMatches(projectColorPattern, "must be a hex color such as #EF233C or #E23"),
	}
}

// projectView is the part of a project the provider exposes. The generated
// response types are anonymous structs, so this gives the resource, the data
// source and the tests one named shape to share.
type projectView struct {
	ID          string
	UID         string
	Name        string
	Description *string
	Region      string
	ClusterID   string
	CreatedAt   time.Time
}

// getProject fetches a project, retrying transient failures. Non-2xx
// responses come back as *northflank.APIError, so callers can tell a missing
// project from a real failure.
func getProject(ctx context.Context, client *northflank.ClientWithResponses, projectID string) (*projectView, error) {
	var view *projectView

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsProjectidWithResponse(ctx, projectID)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		view, err = projectFromGet(got)
		return err
	})

	return view, err
}

func projectFromGet(got *northflank.GetProjectsProjectidHTTPResponse) (*projectView, error) {
	if got.JSON200 == nil {
		return nil, errors.New("the API returned a successful status with no project body")
	}

	data := got.JSON200.Data

	return &projectView{
		ID:          data.Id,
		UID:         data.Uid,
		Name:        data.Name,
		Description: data.Description,
		Region:      data.Deployment.Region,
		ClusterID:   data.Cluster.Id,
		CreatedAt:   data.CreatedAt,
	}, nil
}

// isNotFound reports whether err is the API saying the resource is gone.
func isNotFound(err error) bool {
	var apiErr *northflank.APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

func timeValue(t time.Time) types.String {
	if t.IsZero() {
		return types.StringNull()
	}

	return types.StringValue(t.UTC().Format(time.RFC3339))
}
