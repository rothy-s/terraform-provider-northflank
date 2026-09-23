package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func healthCheckObject(checkType string, successThreshold types.Int64) types.Object {
	return types.ObjectValueMust(serviceHealthCheckAttrTypes, map[string]attr.Value{
		"type":                  types.StringValue(checkType),
		"protocol":              types.StringValue("HTTP"),
		"path":                  types.StringValue("/health"),
		"port":                  types.Int64Value(8080),
		"cmd":                   types.StringNull(),
		"initial_delay_seconds": types.Int64Value(5),
		"period_seconds":        types.Int64Value(60),
		"timeout_seconds":       types.Int64Value(10),
		"failure_threshold":     types.Int64Value(3),
		"success_threshold":     successThreshold,
	})
}

// Regression: Northflank rejected an update with "successThreshold is only
// allowed when 'type' is set to 'readinessProbe'", because the provider sent
// a default success threshold on a liveness probe.
func TestLivenessProbeSendsNoSuccessThreshold(t *testing.T) {
	ctx := t.Context()
	list := types.ListValueMust(types.ObjectType{AttrTypes: serviceHealthCheckAttrTypes}, []attr.Value{
		healthCheckObject("livenessProbe", types.Int64Null()),
		healthCheckObject("readinessProbe", types.Int64Value(1)),
	})

	checks, diags := healthChecksPayload(ctx, list)

	if diags.HasError() {
		t.Fatal(diags)
	}

	raw, _ := json.Marshal(checks)

	var sent []map[string]any
	json.Unmarshal(raw, &sent)

	if _, ok := sent[0]["successThreshold"]; ok {
		t.Errorf("liveness probe = %v, want no successThreshold", sent[0])
	}

	if sent[1]["successThreshold"] != float64(1) {
		t.Errorf("readiness probe = %v, want its successThreshold", sent[1])
	}
}

func TestHealthCheckSuccessThresholdValidator(t *testing.T) {
	ctx := t.Context()

	for name, tt := range map[string]struct {
		check   types.Object
		wantErr bool
	}{
		"readiness with threshold":    {healthCheckObject("readinessProbe", types.Int64Value(2)), false},
		"readiness without threshold": {healthCheckObject("readinessProbe", types.Int64Null()), false},
		"liveness without threshold":  {healthCheckObject("livenessProbe", types.Int64Null()), false},
		"liveness with threshold":     {healthCheckObject("livenessProbe", types.Int64Value(1)), true},
		"startup with threshold":      {healthCheckObject("startupProbe", types.Int64Value(1)), true},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &validator.ObjectResponse{}
			healthCheckSuccessThresholdValidator{}.ValidateObject(ctx, validator.ObjectRequest{
				Path: path.Root("health_checks").AtListIndex(0), ConfigValue: tt.check,
			}, resp)

			if resp.Diagnostics.HasError() != tt.wantErr {
				t.Errorf("diagnostics = %v, want error %v", resp.Diagnostics, tt.wantErr)
			}

			if tt.wantErr && !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "readinessProbe") {
				t.Errorf("detail = %q, want it to name the rule", resp.Diagnostics.Errors()[0].Detail())
			}
		})
	}
}

// Northflank may report a default success threshold for a readiness probe
// that did not set one; recording it would make the apply inconsistent.
func TestHealthChecksReadBackKeepsUnsetThreshold(t *testing.T) {
	ctx := t.Context()
	one, path, port := 1, "/health", 8080

	prior := types.ListValueMust(types.ObjectType{AttrTypes: serviceHealthCheckAttrTypes}, []attr.Value{
		healthCheckObject("readinessProbe", types.Int64Null()),
	})

	reported := []serviceHealthCheckPayload{{
		Type: "readinessProbe", Protocol: "HTTP", Path: &path, Port: &port,
		InitialDelaySeconds: 5, PeriodSeconds: 60, TimeoutSeconds: 10, FailureThreshold: 3, SuccessThreshold: &one,
	}}

	value, diags := healthChecksValue(ctx, reported, prior)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if !value.Equal(prior) {
		t.Errorf("health checks = %v, want the unset threshold kept unset", value)
	}

	// After an import there is nothing configured: record what is reported.
	value, _ = healthChecksValue(ctx, reported, types.ListNull(types.ObjectType{AttrTypes: serviceHealthCheckAttrTypes}))

	var checks []serviceHealthCheckModel
	value.ElementsAs(ctx, &checks, false)

	if checks[0].SuccessThreshold.ValueInt64() != 1 {
		t.Errorf("imported success_threshold = %v, want 1", checks[0].SuccessThreshold)
	}
}

func TestPortsValueKeepsDisableNfDomain(t *testing.T) {
	ctx := t.Context()
	disabled := true
	prior, _ := portsValue(ctx, []servicePortPayload{{Name: "http", InternalPort: 8080, Protocol: "HTTP", Public: true,
		DisableNfDomain: &disabled}}, types.ListNull(types.ObjectType{AttrTypes: servicePortAttrTypes}))

	reported := []servicePortPayload{{Name: "http", InternalPort: 8080, Protocol: "HTTP", Public: true}}

	for name, tc := range map[string]struct {
		prior types.List
		want  bool
	}{
		"omitted, kept from prior": {prior, true},
		"omitted, no prior":        {types.ListNull(types.ObjectType{AttrTypes: servicePortAttrTypes}), false},
	} {
		got, diags := portsValue(ctx, reported, tc.prior)

		if diags.HasError() {
			t.Fatal(diags)
		}

		var models []servicePortModel
		got.ElementsAs(ctx, &models, false)

		if models[0].DisableNfDomain.ValueBool() != tc.want {
			t.Errorf("%s: disable_nf_domain = %v, want %v", name, models[0].DisableNfDomain, tc.want)
		}
	}
}
