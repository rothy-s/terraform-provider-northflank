package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func autoscalingObject(t *testing.T, min, max int64, cpu *int64, withCustom bool) types.Object {
	t.Helper()

	custom := types.ObjectNull(customMetricsAttrTypes)

	if withCustom {
		metric := types.ObjectValueMust(customMetricAttrTypes, map[string]attr.Value{
			"name":      types.StringValue("queue_depth"),
			"type":      types.StringValue("gauge"),
			"threshold": types.Float64Value(2.5),
		})
		custom = types.ObjectValueMust(customMetricsAttrTypes, map[string]attr.Value{
			"path":    types.StringValue("/metrics"),
			"port":    types.Int64Value(9090),
			"metrics": types.ListValueMust(types.ObjectType{AttrTypes: customMetricAttrTypes}, []attr.Value{metric}),
		})
	}

	return types.ObjectValueMust(autoscalingAttrTypes, map[string]attr.Value{
		"min_replicas":                  types.Int64Value(min),
		"max_replicas":                  types.Int64Value(max),
		"cpu_threshold_percentage":      types.Int64PointerValue(cpu),
		"memory_threshold_percentage":   types.Int64Null(),
		"requests_per_second_threshold": types.Int64Value(50),
		"custom_metrics":                custom,
	})
}

func TestAutoscalingPayload(t *testing.T) {
	ctx := t.Context()
	cpu := int64(70)

	payload, diags := autoscalingPayloadFrom(ctx, autoscalingObject(t, 2, 6, &cpu, true), 1)

	if diags.HasError() {
		t.Fatal(diags)
	}

	got, _ := json.Marshal(payload)
	want := `{"horizontal":{"enabled":true,"minReplicas":2,"maxReplicas":6,` +
		`"cpu":{"enabled":true,"thresholdPercentage":70},` +
		// Left out of configuration, so explicitly switched off.
		`"memory":{"enabled":false,"thresholdPercentage":80},` +
		`"rps":{"enabled":true,"thresholdValue":50},` +
		`"userMetrics":{"enabled":true,"exposedMetricsPath":"/metrics","exposedMetricsPort":9090,` +
		`"metrics":[{"metricName":"queue_depth","metricType":"gauge","thresholdValue":2.5}]}}}`

	if string(got) != want {
		t.Errorf("payload =\n%s\nwant\n%s", got, want)
	}

	t.Run("no block switches autoscaling off", func(t *testing.T) {
		for instances, replicas := range map[int64]int{3: 3, 0: 1} {
			payload, _ := autoscalingPayloadFrom(ctx, types.ObjectNull(autoscalingAttrTypes), instances)

			if h := payload.Horizontal; h.Enabled || h.MinReplicas != replicas || h.MaxReplicas != replicas {
				t.Errorf("instances %d: horizontal = %+v, want disabled with %d replicas", instances, h, replicas)
			}
		}
	})
}

// What was applied must read back without a difference, and the payload must
// survive a round trip through the generated types.
func TestAutoscalingRoundTrip(t *testing.T) {
	ctx := t.Context()
	cpu := int64(70)

	for name, configured := range map[string]types.Object{
		"all triggers": autoscalingObject(t, 2, 6, &cpu, true),
		"some":         autoscalingObject(t, 1, 3, nil, false),
		"off":          types.ObjectNull(autoscalingAttrTypes),
	} {
		t.Run(name, func(t *testing.T) {
			payload, diags := autoscalingPayloadFrom(ctx, configured, 2)

			if diags.HasError() {
				t.Fatal(diags)
			}

			raw, _ := json.Marshal(payload)

			var reported autoscalingPayload
			json.Unmarshal(raw, &reported)

			value, diags := autoscalingValue(ctx, &reported)

			if diags.HasError() {
				t.Fatal(diags)
			}

			if !value.Equal(configured) {
				t.Errorf("read back %v, want %v", value, configured)
			}

			plan := servicePlan(t)
			plan.Autoscaling = configured
			body, _ := deploymentServiceRequestBody(ctx, &plan)
			roundTrip(t, body, &northflank.PutProjectsServicesDeploymentJSONRequestBody{})
		})
	}
}

// While autoscaling is on, the reported instance count belongs to the
// autoscaler and must not show as drift.
func TestInstancesIgnoredWhileAutoscaling(t *testing.T) {
	ctx := t.Context()
	cpu := int64(70)

	service, err := serviceFromBody([]byte(serviceResponse))

	if err != nil {
		t.Fatal(err)
	}

	service.Autoscaling = &autoscalingPayload{Horizontal: horizontalAutoscalingPayload{
		Enabled: true, MinReplicas: 2, MaxReplicas: 6,
		CPU: &percentageTriggerPayload{Enabled: true, ThresholdPercentage: 70},
		RPS: &valueTriggerPayload{Enabled: true, ThresholdValue: 50},
	}}
	service.Deployment.Instances = 5 // scaled up by the autoscaler

	model := servicePlan(t)
	model.Instances = types.Int64Value(2)
	model.Autoscaling = autoscalingObject(t, 2, 6, &cpu, false)

	applyDeploymentService(ctx, &model, service)

	if model.Instances.ValueInt64() != 2 {
		t.Errorf("instances = %v, want the configured value kept while autoscaling", model.Instances)
	}

	service.Autoscaling = nil
	applyDeploymentService(ctx, &model, service)

	if model.Instances.ValueInt64() != 5 {
		t.Errorf("instances = %v, want the reported value once autoscaling is off", model.Instances)
	}
}

func TestAutoscalingConfigValidator(t *testing.T) {
	ctx := t.Context()
	cpu := int64(70)

	for name, tt := range map[string]struct {
		min, max int64
		wantErr  bool
	}{
		"equal":    {2, 2, false},
		"range":    {1, 5, false},
		"inverted": {5, 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &validator.ObjectResponse{}
			autoscalingConfigValidator{}.ValidateObject(ctx, validator.ObjectRequest{
				Path:        path.Root("autoscaling"),
				ConfigValue: autoscalingObject(t, tt.min, tt.max, &cpu, false),
			}, resp)

			if resp.Diagnostics.HasError() != tt.wantErr {
				t.Errorf("diagnostics = %v, want error %v", resp.Diagnostics, tt.wantErr)
			}
		})
	}
}

func TestAutoscalingConfigValidatorNeedsTrigger(t *testing.T) {
	ctx := t.Context()

	noTrigger := types.ObjectValueMust(autoscalingAttrTypes, map[string]attr.Value{
		"min_replicas":                  types.Int64Value(1),
		"max_replicas":                  types.Int64Value(3),
		"cpu_threshold_percentage":      types.Int64Null(),
		"memory_threshold_percentage":   types.Int64Null(),
		"requests_per_second_threshold": types.Int64Null(),
		"custom_metrics":                types.ObjectNull(customMetricsAttrTypes),
	})

	resp := &validator.ObjectResponse{}
	autoscalingConfigValidator{}.ValidateObject(ctx, validator.ObjectRequest{
		Path: path.Root("autoscaling"), ConfigValue: noTrigger,
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Error("a block without triggers was accepted")
	}

	// A trigger that is only known at apply time may well be set.
	attrs := noTrigger.Attributes()
	attrs["cpu_threshold_percentage"] = types.Int64Unknown()

	resp = &validator.ObjectResponse{}
	autoscalingConfigValidator{}.ValidateObject(ctx, validator.ObjectRequest{
		Path: path.Root("autoscaling"), ConfigValue: types.ObjectValueMust(autoscalingAttrTypes, attrs),
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Errorf("an unknown trigger was rejected: %v", resp.Diagnostics)
	}
}
