package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Addon states, as the API reports them, grouped by what the provider does
// about them while waiting.
var (
	// addonReadyStates end a wait successfully.
	addonReadyStates = map[string]bool{"running": true, "paused": true}

	// addonFailedStates end a wait with an error: Northflank will not get the
	// addon to running on its own.
	addonFailedStates = map[string]bool{"failed": true, "error": true, "errorAllocating": true}
)

// How long to wait for an addon, and how often to look. Variables so that
// tests can shorten them.
var (
	addonReadyTimeout  = 30 * time.Minute
	addonDeleteTimeout = 15 * time.Minute
	addonPollInterval  = 10 * time.Second
)

// addonRead is the part of the get-addon response the provider uses. The
// response nests the configuration in a union over the three addon kinds, so
// the body is decoded by hand, as for services.
type addonRead struct {
	ID          string    `json:"id"`
	UID         string    `json:"uid"`
	AppID       string    `json:"appId"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Tags        []string  `json:"tags"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`

	Spec struct {
		Type   string `json:"type"`
		Config struct {
			VersionTag string `json:"versionTag"`
			Deployment *struct {
				PlanID       string  `json:"planId"`
				Region       string  `json:"region"`
				Replicas     int     `json:"replicas"`
				StorageClass string  `json:"storageClass"`
				StorageSize  float64 `json:"storageSize"`
			} `json:"deployment"`
			Networking *struct {
				ExternalAccessEnabled bool `json:"externalAccessEnabled"`
				TLSEnabled            bool `json:"tlsEnabled"`
				VpcAccessible         bool `json:"vpcAccessible"`
			} `json:"networking"`
		} `json:"config"`
	} `json:"spec"`
}

// addonCredentials is what the credentials endpoint reports. Both maps depend
// on the addon type.
type addonCredentials struct {
	Secrets map[string]string
	Envs    map[string]string
}

// ---- API payloads ----------------------------------------------------------

type addonCreatePayload struct {
	Name                  string                  `json:"name"`
	Description           *string                 `json:"description,omitempty"`
	Tags                  []string                `json:"tags"`
	Type                  string                  `json:"type"`
	Version               string                  `json:"version"`
	Billing               addonBillingPayload     `json:"billing"`
	TLSEnabled            bool                    `json:"tlsEnabled"`
	ExternalAccessEnabled bool                    `json:"externalAccessEnabled"`
	VpcAccessible         bool                    `json:"vpcAccessible"`
	CustomCredentials     *addonCredentialPayload `json:"customCredentials,omitempty"`
}

type addonBillingPayload struct {
	DeploymentPlan string  `json:"deploymentPlan"`
	Replicas       int     `json:"replicas"`
	Storage        int     `json:"storage"`
	StorageClass   *string `json:"storageClass,omitempty"`
}

type addonCredentialPayload struct {
	DBName string `json:"dbName"`
}

// addonPatchPayload carries only what changed, so that an update touches as
// little of a running database as possible.
type addonPatchPayload struct {
	Description           *string                   `json:"description,omitempty"`
	Tags                  *[]string                 `json:"tags,omitempty"`
	Version               *string                   `json:"version,omitempty"`
	Billing               *addonBillingPatchPayload `json:"billing,omitempty"`
	TLSEnabled            *bool                     `json:"tlsEnabled,omitempty"`
	ExternalAccessEnabled *bool                     `json:"externalAccessEnabled,omitempty"`
	VpcAccessible         *bool                     `json:"vpcAccessible,omitempty"`
}

type addonBillingPatchPayload struct {
	DeploymentPlan *string `json:"deploymentPlan,omitempty"`
	Replicas       *int    `json:"replicas,omitempty"`
	Storage        *int    `json:"storage,omitempty"`
	StorageClass   *string `json:"storageClass,omitempty"`
}

func (p *addonPatchPayload) empty() bool {
	return *p == addonPatchPayload{}
}

// ---- API calls -------------------------------------------------------------

// getAddon fetches an addon, retrying transient failures.
func getAddon(ctx context.Context, client *northflank.ClientWithResponses, projectID, addonID string) (*addonRead, error) {
	var addon *addonRead

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsAddonsAddonidWithResponse(ctx, projectID, addonID)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		addon, err = addonFromBody(got.Body)
		return err
	})

	return addon, err
}

func addonFromBody(body []byte) (*addonRead, error) {
	var envelope struct {
		Data *addonRead `json:"data"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding addon: %w", err)
	}

	if envelope.Data == nil || envelope.Data.ID == "" {
		return nil, errors.New("the API returned a successful status with no addon body")
	}

	return envelope.Data, nil
}

// getAddonCredentials fetches an addon's connection details. Values that are
// not strings are encoded as JSON, since Terraform maps hold one type.
func getAddonCredentials(ctx context.Context, client *northflank.ClientWithResponses, projectID, addonID string) (*addonCredentials, error) {
	var creds *addonCredentials

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsAddonsAddonidCredentialsWithResponse(ctx, projectID, addonID)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		var envelope struct {
			Data struct {
				Secrets map[string]any `json:"secrets"`
				Envs    map[string]any `json:"envs"`
			} `json:"data"`
		}

		if err := json.Unmarshal(got.Body, &envelope); err != nil {
			return fmt.Errorf("decoding addon credentials: %w", err)
		}

		creds = &addonCredentials{
			Secrets: stringifyValues(envelope.Data.Secrets),
			Envs:    stringifyValues(envelope.Data.Envs),
		}

		return nil
	})

	return creds, err
}

// waitForAddon polls until the addon reaches a ready state, fails, or the
// timeout passes. It returns the last addon it read.
func waitForAddon(ctx context.Context, client *northflank.ClientWithResponses, projectID, addonID string) (*addonRead, error) {
	ctx, cancel := context.WithTimeout(ctx, addonReadyTimeout)
	defer cancel()

	lastStatus := "unknown"

	for {
		addon, err := getAddon(ctx, client, projectID, addonID)

		// The deadline can pass mid-request; report that as the timeout it
		// is, not as the HTTP client's context error.
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("timed out after %s waiting for addon %q to be running; it was %q",
				addonReadyTimeout, addonID, lastStatus)
		}

		if err != nil {
			return nil, err
		}

		lastStatus = addon.Status

		switch {
		case addonReadyStates[addon.Status]:
			return addon, nil
		case addonFailedStates[addon.Status]:
			return addon, fmt.Errorf("addon %q is in state %q", addonID, addon.Status)
		}

		select {
		case <-ctx.Done():
			return addon, fmt.Errorf("timed out after %s waiting for addon %q to be running; it was %q",
				addonReadyTimeout, addonID, addon.Status)
		case <-time.After(addonPollInterval):
		}
	}
}

// waitForAddonDeleted polls until the addon is gone. Deleting is
// asynchronous, and a project cannot be deleted while it still has an addon.
func waitForAddonDeleted(ctx context.Context, client *northflank.ClientWithResponses, projectID, addonID string) error {
	ctx, cancel := context.WithTimeout(ctx, addonDeleteTimeout)
	defer cancel()

	for {
		_, err := getAddon(ctx, client, projectID, addonID)

		if isNotFound(err) {
			return nil
		}

		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s waiting for addon %q to be deleted", addonDeleteTimeout, addonID)
		}

		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s waiting for addon %q to be deleted", addonDeleteTimeout, addonID)
		case <-time.After(addonPollInterval):
		}
	}
}

// ---- Conversions -----------------------------------------------------------

// credentialMaps converts credentials into the two sensitive maps. Missing
// credentials, for instance on an addon that is not running, read as null.
func credentialMaps(ctx context.Context, creds *addonCredentials) (types.Map, types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics

	if creds == nil {
		return types.MapNull(types.StringType), types.MapNull(types.StringType), nil
	}

	secrets := stringMapOrNull(ctx, creds.Secrets, &diags)
	envs := stringMapOrNull(ctx, creds.Envs, &diags)

	return secrets, envs, diags
}

func stringifyValues(values map[string]any) map[string]string {
	out := make(map[string]string, len(values))

	for k, v := range values {
		switch s := v.(type) {
		case string:
			out[k] = s
		case nil:
			// Leave absent rather than record "null".
		default:
			encoded, _ := json.Marshal(s)
			out[k] = string(encoded)
		}
	}

	return out
}
