package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Enumerations and constraints copied from the OpenAPI document.
var (
	servicePortProtocols        = []string{"HTTP", "HTTP/2", "TCP", "UDP"}
	serviceHealthCheckTypes     = []string{"livenessProbe", "readinessProbe", "startupProbe"}
	serviceHealthCheckProtocols = []string{"HTTP", "CMD", "TCP"}

	servicePortNamePattern = regexp.MustCompile(`^[a-zA-Z](-?[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*)?$`)
)

const serviceDefaultInstances = 1

// serviceRead is the part of the get-service response the provider uses.
//
// The response is a union over the three service kinds, and the generated
// types wrap several plain strings (port protocols, build service IDs, commit
// SHAs) in union types of their own, which makes them awkward to read. The
// body is decoded into this struct instead; a test checks it against a
// response in the documented shape.
type serviceRead struct {
	ID          string     `json:"id"`
	UID         string     `json:"uid"`
	AppID       string     `json:"appId"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	Tags        []string   `json:"tags"`
	ServiceType string     `json:"serviceType"`
	CreatedAt   *time.Time `json:"createdAt"`
	UpdatedAt   *time.Time `json:"updatedAt"`

	Billing struct {
		DeploymentPlan string  `json:"deploymentPlan"`
		BuildPlan      *string `json:"buildPlan"`
	} `json:"billing"`

	// Build and combined services only.
	VcsData *struct {
		ProjectURL      string  `json:"projectUrl"`
		ProjectType     string  `json:"projectType"`
		ProjectBranch   *string `json:"projectBranch"`
		AccountLogin    *string `json:"accountLogin"`
		VcsLinkID       *string `json:"vcsLinkId"`
		SelfHostedVcsID *string `json:"selfHostedVcsId"`
	} `json:"vcsData"`
	BuildSettings *struct {
		Dockerfile *struct {
			BuildEngine    string `json:"buildEngine"`
			DockerFilePath string `json:"dockerFilePath"`
			DockerWorkDir  string `json:"dockerWorkDir"`
			Buildkit       *struct {
				UseCache bool `json:"useCache"`
			} `json:"buildkit"`
			Storage *buildStoragePayload `json:"storage"`
		} `json:"dockerfile"`
		Buildpack *struct {
			Builder           string               `json:"builder"`
			BuildContext      string               `json:"buildContext"`
			BuildpackLocators []string             `json:"buildpackLocators"`
			UseCache          bool                 `json:"useCache"`
			Storage           *buildStoragePayload `json:"storage"`
		} `json:"buildpack"`
	} `json:"buildSettings"`
	BuildConfiguration *struct {
		DockerfileTarget   *string  `json:"dockerfileTarget"`
		BranchRestrictions []string `json:"branchRestrictions"`
		PrRestrictions     []string `json:"prRestrictions"`
	} `json:"buildConfiguration"`
	BuildArguments map[string]string `json:"buildArguments"`
	DisabledCI     bool              `json:"disabledCI"`

	Deployment *struct {
		Instances int `json:"instances"`
		External  *struct {
			ImagePath   string  `json:"imagePath"`
			Credentials *string `json:"credentials"`
		} `json:"external"`
		Internal *struct {
			ID       string  `json:"id"`
			Branch   *string `json:"branch"`
			BuildSHA *string `json:"buildSHA"`
		} `json:"internal"`
		Docker *struct {
			ConfigType       string  `json:"configType"`
			CustomCommand    *string `json:"customCommand"`
			CustomEntrypoint *string `json:"customEntrypoint"`
		} `json:"docker"`
	} `json:"deployment"`

	Autoscaling        *autoscalingPayload         `json:"autoscaling"`
	Ports              []servicePortPayload        `json:"ports"`
	HealthChecks       []serviceHealthCheckPayload `json:"healthChecks"`
	RuntimeEnvironment map[string]string           `json:"runtimeEnvironment"`
}

// servicePortPayload is one port, in the shape the API both takes and
// reports. DisableNfDomain is a pointer so that a response leaving it out can
// be told apart from false.
type servicePortPayload struct {
	Name            string   `json:"name"`
	InternalPort    int      `json:"internalPort"`
	Protocol        string   `json:"protocol"`
	Public          bool     `json:"public"`
	Domains         []string `json:"domains"`
	DisableNfDomain *bool    `json:"disableNfDomain,omitempty"`
}

// serviceHealthCheckPayload is one health check, in the shape the API both
// takes and reports.
type serviceHealthCheckPayload struct {
	Type                string  `json:"type"`
	Protocol            string  `json:"protocol"`
	Path                *string `json:"path,omitempty"`
	Port                *int    `json:"port,omitempty"`
	Cmd                 *string `json:"cmd,omitempty"`
	InitialDelaySeconds int     `json:"initialDelaySeconds"`
	PeriodSeconds       int     `json:"periodSeconds"`
	TimeoutSeconds      int     `json:"timeoutSeconds"`
	FailureThreshold    int     `json:"failureThreshold"`
	SuccessThreshold    *int    `json:"successThreshold,omitempty"`
}

// getService fetches a service of any kind, retrying transient failures.
func getService(ctx context.Context, client *northflank.ClientWithResponses, projectID, serviceID string) (*serviceRead, error) {
	var view *serviceRead

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetProjectsServicesServiceidWithResponse(ctx, projectID, serviceID)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		view, err = serviceFromBody(got.Body)
		return err
	})

	return view, err
}

func serviceFromBody(body []byte) (*serviceRead, error) {
	var envelope struct {
		Data *serviceRead `json:"data"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding service: %w", err)
	}

	if envelope.Data == nil {
		return nil, errors.New("the API returned a successful status with no service body")
	}

	return envelope.Data, nil
}

// command and entrypoint unpack the docker override the way the API reports
// it, the inverse of dockerOverride.
func (s *serviceRead) command() *string {
	if s.Deployment == nil || s.Deployment.Docker == nil {
		return nil
	}

	switch s.Deployment.Docker.ConfigType {
	case "customCommand", "customEntrypointCustomCommand":
		return s.Deployment.Docker.CustomCommand
	}

	return nil
}

func (s *serviceRead) entrypoint() *string {
	if s.Deployment == nil || s.Deployment.Docker == nil {
		return nil
	}

	switch s.Deployment.Docker.ConfigType {
	case "customEntrypoint", "customEntrypointCustomCommand":
		return s.Deployment.Docker.CustomEntrypoint
	}

	return nil
}

var (
	servicePortAttrTypes = map[string]attr.Type{
		"name":          types.StringType,
		"internal_port": types.Int64Type,
		"protocol":      types.StringType,
		"public":        types.BoolType,
		"domains":       types.SetType{ElemType: types.StringType},

		"disable_nf_domain": types.BoolType,
	}

	serviceHealthCheckAttrTypes = map[string]attr.Type{
		"type":                  types.StringType,
		"protocol":              types.StringType,
		"path":                  types.StringType,
		"port":                  types.Int64Type,
		"cmd":                   types.StringType,
		"initial_delay_seconds": types.Int64Type,
		"period_seconds":        types.Int64Type,
		"timeout_seconds":       types.Int64Type,
		"failure_threshold":     types.Int64Type,
		"success_threshold":     types.Int64Type,
	}
)

type servicePortModel struct {
	Name         types.String `tfsdk:"name"`
	InternalPort types.Int64  `tfsdk:"internal_port"`
	Protocol     types.String `tfsdk:"protocol"`
	Public       types.Bool   `tfsdk:"public"`
	Domains      types.Set    `tfsdk:"domains"`

	DisableNfDomain types.Bool `tfsdk:"disable_nf_domain"`
}

type serviceHealthCheckModel struct {
	Type                types.String `tfsdk:"type"`
	Protocol            types.String `tfsdk:"protocol"`
	Path                types.String `tfsdk:"path"`
	Port                types.Int64  `tfsdk:"port"`
	Cmd                 types.String `tfsdk:"cmd"`
	InitialDelaySeconds types.Int64  `tfsdk:"initial_delay_seconds"`
	PeriodSeconds       types.Int64  `tfsdk:"period_seconds"`
	TimeoutSeconds      types.Int64  `tfsdk:"timeout_seconds"`
	FailureThreshold    types.Int64  `tfsdk:"failure_threshold"`
	SuccessThreshold    types.Int64  `tfsdk:"success_threshold"`
}

// portsValue converts reported ports into Terraform's list. No ports is an
// empty list, matching the schema default. Where Northflank leaves out
// disableNfDomain, the prior port's value is kept, or false without one.
func portsValue(ctx context.Context, ports []servicePortPayload, prior types.List) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics
	var priorPorts []servicePortModel

	if !prior.IsNull() && !prior.IsUnknown() {
		diags.Append(prior.ElementsAs(ctx, &priorPorts, false)...)
	}

	models := make([]servicePortModel, 0, len(ports))

	for i, p := range ports {
		disableNfDomain := types.BoolValue(false)

		switch {
		case p.DisableNfDomain != nil:
			disableNfDomain = types.BoolValue(*p.DisableNfDomain)
		case i < len(priorPorts) && !priorPorts[i].DisableNfDomain.IsNull() && !priorPorts[i].DisableNfDomain.IsUnknown():
			disableNfDomain = priorPorts[i].DisableNfDomain
		}

		models = append(models, servicePortModel{
			Name:            types.StringValue(p.Name),
			InternalPort:    types.Int64Value(int64(p.InternalPort)),
			Protocol:        types.StringValue(p.Protocol),
			Public:          types.BoolValue(p.Public),
			Domains:         setOrEmpty(ctx, p.Domains, &diags),
			DisableNfDomain: disableNfDomain,
		})
	}

	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: servicePortAttrTypes}, models)
	diags.Append(d...)

	return list, diags
}

func portsPayload(ctx context.Context, value types.List) ([]servicePortPayload, diag.Diagnostics) {
	out := []servicePortPayload{}

	if value.IsNull() || value.IsUnknown() {
		return out, nil
	}

	var models []servicePortModel
	diags := value.ElementsAs(ctx, &models, false)

	for _, m := range models {
		out = append(out, servicePortPayload{
			Name:         m.Name.ValueString(),
			InternalPort: int(m.InternalPort.ValueInt64()),
			Protocol:     m.Protocol.ValueString(),
			Public:       m.Public.ValueBool(),
			Domains:      stringsFromSet(ctx, m.Domains, &diags),

			DisableNfDomain: m.DisableNfDomain.ValueBoolPointer(),
		})
	}

	return out, diags
}

// healthChecksValue converts reported health checks into Terraform's list.
// A success threshold the prior list leaves unset stays unset: Northflank may
// report its own default for readiness probes, and recording it would make
// the apply come back inconsistent.
func healthChecksValue(ctx context.Context, checks []serviceHealthCheckPayload, prior types.List) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics
	var priorChecks []serviceHealthCheckModel

	if !prior.IsNull() && !prior.IsUnknown() {
		diags.Append(prior.ElementsAs(ctx, &priorChecks, false)...)
	}

	models := make([]serviceHealthCheckModel, 0, len(checks))

	for i, c := range checks {
		successThreshold := int64PointerValue(c.SuccessThreshold)

		if i < len(priorChecks) && priorChecks[i].SuccessThreshold.IsNull() {
			successThreshold = types.Int64Null()
		}

		models = append(models, serviceHealthCheckModel{
			Type:                types.StringValue(c.Type),
			Protocol:            types.StringValue(c.Protocol),
			Path:                types.StringPointerValue(c.Path),
			Port:                int64PointerValue(c.Port),
			Cmd:                 types.StringPointerValue(c.Cmd),
			InitialDelaySeconds: types.Int64Value(int64(c.InitialDelaySeconds)),
			PeriodSeconds:       types.Int64Value(int64(c.PeriodSeconds)),
			TimeoutSeconds:      types.Int64Value(int64(c.TimeoutSeconds)),
			FailureThreshold:    types.Int64Value(int64(c.FailureThreshold)),
			SuccessThreshold:    successThreshold,
		})
	}

	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: serviceHealthCheckAttrTypes}, models)
	diags.Append(d...)

	return list, diags
}

// healthCheckSuccessThresholdValidator rejects success_threshold on anything
// but a readiness probe at plan time, which Northflank otherwise rejects
// halfway through an apply.
type healthCheckSuccessThresholdValidator struct{}

func (healthCheckSuccessThresholdValidator) Description(context.Context) string {
	return "success_threshold is only allowed on a readinessProbe"
}

func (v healthCheckSuccessThresholdValidator) MarkdownDescription(ctx context.Context) string {
	return "`success_threshold` is only allowed on a `readinessProbe`"
}

func (healthCheckSuccessThresholdValidator) ValidateObject(ctx context.Context, req validator.ObjectRequest, resp *validator.ObjectResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	attrs := req.ConfigValue.Attributes()
	threshold, _ := attrs["success_threshold"].(types.Int64)
	checkType, _ := attrs["type"].(types.String)

	if threshold.IsNull() || threshold.IsUnknown() || checkType.IsUnknown() {
		return
	}

	if checkType.ValueString() != "readinessProbe" {
		resp.Diagnostics.AddAttributeError(
			req.Path.AtName("success_threshold"),
			"success_threshold not allowed",
			fmt.Sprintf("success_threshold is only allowed on a readinessProbe, not on a %s. Remove it.", checkType.ValueString()),
		)
	}
}

func healthChecksPayload(ctx context.Context, value types.List) ([]serviceHealthCheckPayload, diag.Diagnostics) {
	out := []serviceHealthCheckPayload{}

	if value.IsNull() || value.IsUnknown() {
		return out, nil
	}

	var models []serviceHealthCheckModel
	diags := value.ElementsAs(ctx, &models, false)

	for _, m := range models {
		out = append(out, serviceHealthCheckPayload{
			Type:                m.Type.ValueString(),
			Protocol:            m.Protocol.ValueString(),
			Path:                knownStringPointer(m.Path),
			Port:                knownIntPointer(m.Port),
			Cmd:                 knownStringPointer(m.Cmd),
			InitialDelaySeconds: int(m.InitialDelaySeconds.ValueInt64()),
			PeriodSeconds:       int(m.PeriodSeconds.ValueInt64()),
			TimeoutSeconds:      int(m.TimeoutSeconds.ValueInt64()),
			FailureThreshold:    int(m.FailureThreshold.ValueInt64()),
			SuccessThreshold:    knownIntPointer(m.SuccessThreshold),
		})
	}

	return out, diags
}

func setOrEmpty(ctx context.Context, values []string, diags *diag.Diagnostics) types.Set {
	set, d := types.SetValueFrom(ctx, types.StringType, nonNil(values))
	diags.Append(d...)

	return set
}

func int64PointerValue(p *int) types.Int64 {
	if p == nil {
		return types.Int64Null()
	}

	return types.Int64Value(int64(*p))
}

func knownIntPointer(v types.Int64) *int {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}

	i := int(v.ValueInt64())
	return &i
}

func timePointerValue(t *time.Time) types.String {
	if t == nil {
		return types.StringNull()
	}

	return timeValue(*t)
}

// Service kinds, as the API names them in serviceType and in its paths.
const (
	serviceKindDeployment = "deployment"
	serviceKindBuild      = "build"
	serviceKindCombined   = "combined"
)

// createService creates a service of the given kind and returns what the
// create response reports about it. Creates are not retried: a failure does
// not prove nothing was made.
func createService(ctx context.Context, client *northflank.ClientWithResponses, kind, projectID string, body []byte) (*serviceRead, error) {
	var (
		resp apierr.Response
		err  error
	)

	switch kind {
	case serviceKindDeployment:
		resp, err = client.PostProjectsServicesDeploymentWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))
	case serviceKindBuild:
		resp, err = client.PostProjectsServicesBuildWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))
	case serviceKindCombined:
		resp, err = client.PostProjectsServicesCombinedWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))
	default:
		return nil, fmt.Errorf("unknown service kind %q", kind)
	}

	// The response is only safe to touch once the request itself succeeded.
	if err != nil {
		return nil, err
	}

	if err := northflank.CheckResponse(resp); err != nil {
		return nil, err
	}

	created, err := serviceFromBody(resp.GetBody())

	if err != nil {
		return nil, err
	}

	if created.ID == "" {
		return nil, errors.New("the create response carries no service ID")
	}

	return created, nil
}

// updateService replaces a service of the given kind with PUT, which takes
// the whole service and is idempotent, so it is retried.
func updateService(ctx context.Context, client *northflank.ClientWithResponses, kind, projectID string, body []byte) error {
	return apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		var (
			resp apierr.Response
			err  error
		)

		switch kind {
		case serviceKindDeployment:
			resp, err = client.PutProjectsServicesDeploymentWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))
		case serviceKindBuild:
			resp, err = client.PutProjectsServicesBuildWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))
		case serviceKindCombined:
			resp, err = client.PutProjectsServicesCombinedWithBodyWithResponse(ctx, projectID, "application/json", bytes.NewReader(body))
		default:
			return fmt.Errorf("unknown service kind %q", kind)
		}

		if err != nil {
			return err
		}

		return northflank.CheckResponse(resp)
	})
}

// deleteService deletes a service of any kind. Volumes attached to it are
// left alone: the provider does not manage them, so deleting them would be a
// surprise. A service that is already gone counts as deleted.
func deleteService(ctx context.Context, client *northflank.ClientWithResponses, projectID, serviceID string) error {
	params := &northflank.DeleteProjectsServicesServiceidParams{}

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		deleted, err := client.DeleteProjectsServicesServiceidWithResponse(ctx, projectID, serviceID, params)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(deleted)
	})

	if isNotFound(err) {
		return nil
	}

	return err
}

// checkServiceKind reports an error for a service of another kind, which a
// kind-specific resource must not adopt.
func checkServiceKind(service *serviceRead, kind string) error {
	if service.ServiceType != kind {
		return fmt.Errorf("service %q is a %s service, which northflank_%s_service cannot manage",
			service.ID, service.ServiceType, kind)
	}

	return nil
}

// parseServiceImportID splits <project_id>/<service_id>.
func parseServiceImportID(id, resourceType string) (string, string, error) {
	projectID, serviceID, ok := strings.Cut(id, "/")

	if !ok || projectID == "" || serviceID == "" || strings.Contains(serviceID, "/") {
		return "", "", fmt.Errorf("expected <project_id>/<service_id>, got %q. Import with: "+
			"terraform import %s.example my-project/my-service", id, resourceType)
	}

	return projectID, serviceID, nil
}
