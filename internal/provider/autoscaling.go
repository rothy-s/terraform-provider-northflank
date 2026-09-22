package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Horizontal autoscaling is shared by deployment and combined services, which
// describe it with the same API shape.

var (
	customMetricTypes = []string{"gauge", "counter"}

	customMetricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	metricsPathPattern      = regexp.MustCompile(`^/([_a-zA-Z0-9-&?=.]*)((/[_a-zA-Z0-9-&?=.]+)*(/)?)?$`)
)

// Placeholder thresholds sent with a trigger that is switched off. The API
// requires a threshold on every trigger object it receives, even a disabled
// one; the value has no effect.
const (
	disabledPercentageThreshold = 80
	disabledRPSThreshold        = 1
)

type autoscalingModel struct {
	MinReplicas                types.Int64  `tfsdk:"min_replicas"`
	MaxReplicas                types.Int64  `tfsdk:"max_replicas"`
	CPUThresholdPercentage     types.Int64  `tfsdk:"cpu_threshold_percentage"`
	MemoryThresholdPercentage  types.Int64  `tfsdk:"memory_threshold_percentage"`
	RequestsPerSecondThreshold types.Int64  `tfsdk:"requests_per_second_threshold"`
	CustomMetrics              types.Object `tfsdk:"custom_metrics"`
}

type customMetricsModel struct {
	Path    types.String `tfsdk:"path"`
	Port    types.Int64  `tfsdk:"port"`
	Metrics types.List   `tfsdk:"metrics"`
}

type customMetricModel struct {
	Name      types.String  `tfsdk:"name"`
	Type      types.String  `tfsdk:"type"`
	Threshold types.Float64 `tfsdk:"threshold"`
}

var (
	customMetricAttrTypes = map[string]attr.Type{
		"name":      types.StringType,
		"type":      types.StringType,
		"threshold": types.Float64Type,
	}

	customMetricsAttrTypes = map[string]attr.Type{
		"path":    types.StringType,
		"port":    types.Int64Type,
		"metrics": types.ListType{ElemType: types.ObjectType{AttrTypes: customMetricAttrTypes}},
	}

	autoscalingAttrTypes = map[string]attr.Type{
		"min_replicas":                  types.Int64Type,
		"max_replicas":                  types.Int64Type,
		"cpu_threshold_percentage":      types.Int64Type,
		"memory_threshold_percentage":   types.Int64Type,
		"requests_per_second_threshold": types.Int64Type,
		"custom_metrics":                types.ObjectType{AttrTypes: customMetricsAttrTypes},
	}
)

// ---- API payloads, which are also the reported shape -----------------------

type autoscalingPayload struct {
	Horizontal horizontalAutoscalingPayload `json:"horizontal"`
}

type horizontalAutoscalingPayload struct {
	Enabled     bool                         `json:"enabled"`
	MinReplicas int                          `json:"minReplicas"`
	MaxReplicas int                          `json:"maxReplicas"`
	CPU         *percentageTriggerPayload    `json:"cpu,omitempty"`
	Memory      *percentageTriggerPayload    `json:"memory,omitempty"`
	RPS         *valueTriggerPayload         `json:"rps,omitempty"`
	UserMetrics *customMetricsTriggerPayload `json:"userMetrics,omitempty"`
}

type percentageTriggerPayload struct {
	Enabled             bool `json:"enabled"`
	ThresholdPercentage int  `json:"thresholdPercentage"`
}

type valueTriggerPayload struct {
	Enabled        bool `json:"enabled"`
	ThresholdValue int  `json:"thresholdValue"`
}

type customMetricsTriggerPayload struct {
	Enabled            bool                  `json:"enabled"`
	ExposedMetricsPath string                `json:"exposedMetricsPath"`
	ExposedMetricsPort int                   `json:"exposedMetricsPort"`
	Metrics            []customMetricPayload `json:"metrics"`
}

type customMetricPayload struct {
	MetricName     string  `json:"metricName"`
	MetricType     string  `json:"metricType"`
	ThresholdValue float64 `json:"thresholdValue"`
}

// ---- Schema ----------------------------------------------------------------

func autoscalingAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Scale the number of instances between `min_replicas` and `max_replicas` based on " +
			"load. Set at least one trigger; the service scales up when any of them is exceeded. Leave the " +
			"block out to run a fixed number of `instances`.\n\n" +
			"While autoscaling is on, the autoscaler owns the instance count, so `instances` is not read back.",
		// objectvalidator.AtLeastOneOf would count the block itself, which is
		// always set here, so the trigger check lives in the custom validator.
		Validators: []validator.Object{
			autoscalingConfigValidator{},
		},
		Attributes: map[string]schema.Attribute{
			"min_replicas": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Fewest instances to run, at least 1.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"max_replicas": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Most instances to run. Must not be below `min_replicas`.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"cpu_threshold_percentage": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Scale up when average CPU usage exceeds this percentage of the plan, 10 to 100.",
				Validators: []validator.Int64{
					int64validator.Between(10, 100),
				},
			},
			"memory_threshold_percentage": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Scale up when average memory usage exceeds this percentage of the plan, 10 to 100.",
				Validators: []validator.Int64{
					int64validator.Between(10, 100),
				},
			},
			"requests_per_second_threshold": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Scale up when requests per second per instance exceed this value.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
			},
			"custom_metrics": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Scale on Prometheus metrics the service exposes itself. The API cannot " +
					"switch this trigger off without its settings, so removing the block stops sending it; should " +
					"Northflank keep it anyway, the next plan shows it as a difference.",
				Attributes: map[string]schema.Attribute{
					"path": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Path the metrics are served on, such as `/metrics`.",
						Validators: []validator.String{
							stringvalidator.RegexMatches(metricsPathPattern, "must be an absolute path such as /metrics"),
						},
					},
					"port": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "Port the metrics are served on.",
						Validators: []validator.Int64{
							int64validator.Between(1, 65535),
						},
					},
					"metrics": schema.ListNestedAttribute{
						Required:            true,
						MarkdownDescription: "Metrics to scale on. At least one is required.",
						Validators: []validator.List{
							listvalidator.SizeAtLeast(1),
						},
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"name": schema.StringAttribute{
									Required:            true,
									MarkdownDescription: "Name of the metric, as exposed.",
									Validators: []validator.String{
										stringvalidator.RegexMatches(customMetricNamePattern, "must be a Prometheus metric name"),
									},
								},
								"type": schema.StringAttribute{
									Required: true,
									MarkdownDescription: "`gauge`, used as is, or `counter`, used as a rate per " +
										"second.",
									Validators: []validator.String{
										stringvalidator.OneOf(customMetricTypes...),
									},
								},
								"threshold": schema.Float64Attribute{
									Required:            true,
									MarkdownDescription: "Scale up when the average across instances exceeds this value.",
								},
							},
						},
					},
				},
			},
		},
	}
}

// autoscalingConfigValidator rejects, at plan time rather than halfway
// through an apply, a block with no trigger and a max_replicas below
// min_replicas.
type autoscalingConfigValidator struct{}

func (autoscalingConfigValidator) Description(context.Context) string {
	return "at least one trigger must be set, and max_replicas must not be below min_replicas"
}

func (autoscalingConfigValidator) MarkdownDescription(context.Context) string {
	return "at least one trigger must be set, and `max_replicas` must not be below `min_replicas`"
}

func (autoscalingConfigValidator) ValidateObject(ctx context.Context, req validator.ObjectRequest, resp *validator.ObjectResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}

	var model autoscalingModel
	resp.Diagnostics.Append(req.ConfigValue.As(ctx, &model, objectAsOptions)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown trigger may turn out to be set, so only a definite absence
	// of all of them is an error.
	triggerMaybeSet := false

	for _, v := range []attr.Value{
		model.CPUThresholdPercentage, model.MemoryThresholdPercentage,
		model.RequestsPerSecondThreshold, model.CustomMetrics,
	} {
		if !v.IsNull() {
			triggerMaybeSet = true
		}
	}

	if !triggerMaybeSet {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Missing autoscaling trigger",
			"Set at least one of cpu_threshold_percentage, memory_threshold_percentage, "+
				"requests_per_second_threshold or custom_metrics, or remove the autoscaling block.",
		)
	}

	if model.MinReplicas.IsUnknown() || model.MaxReplicas.IsUnknown() {
		return
	}

	if model.MaxReplicas.ValueInt64() < model.MinReplicas.ValueInt64() {
		resp.Diagnostics.AddAttributeError(
			req.Path.AtName("max_replicas"),
			"Invalid autoscaling range",
			fmt.Sprintf("max_replicas (%d) must not be below min_replicas (%d).",
				model.MaxReplicas.ValueInt64(), model.MinReplicas.ValueInt64()),
		)
	}
}

// ---- Conversions -----------------------------------------------------------

// autoscalingPayloadFrom converts the autoscaling block. Leaving the block out
// switches autoscaling off explicitly, and so does leaving a trigger out, so
// that removing either from configuration takes effect whether or not the API
// merges updates.
func autoscalingPayloadFrom(ctx context.Context, value types.Object, instances int64) (*autoscalingPayload, diag.Diagnostics) {
	var diags diag.Diagnostics

	if value.IsNull() || value.IsUnknown() {
		// minReplicas and maxReplicas are required even when disabled, and
		// minReplicas must be at least 1.
		replicas := int(max(instances, 1))

		return &autoscalingPayload{Horizontal: horizontalAutoscalingPayload{
			Enabled:     false,
			MinReplicas: replicas,
			MaxReplicas: replicas,
		}}, nil
	}

	var model autoscalingModel
	diags.Append(value.As(ctx, &model, objectAsOptions)...)

	if diags.HasError() {
		return nil, diags
	}

	h := horizontalAutoscalingPayload{
		Enabled:     true,
		MinReplicas: int(model.MinReplicas.ValueInt64()),
		MaxReplicas: int(model.MaxReplicas.ValueInt64()),
		CPU:         percentageTrigger(model.CPUThresholdPercentage),
		Memory:      percentageTrigger(model.MemoryThresholdPercentage),
		RPS:         &valueTriggerPayload{Enabled: false, ThresholdValue: disabledRPSThreshold},
	}

	if t := knownIntPointer(model.RequestsPerSecondThreshold); t != nil {
		h.RPS = &valueTriggerPayload{Enabled: true, ThresholdValue: *t}
	}

	// A disabled custom metrics trigger still needs a path, a port and a
	// metric, so it is left out rather than faked.
	if !model.CustomMetrics.IsNull() && !model.CustomMetrics.IsUnknown() {
		var custom customMetricsModel
		diags.Append(model.CustomMetrics.As(ctx, &custom, objectAsOptions)...)

		var metrics []customMetricModel
		diags.Append(custom.Metrics.ElementsAs(ctx, &metrics, false)...)

		trigger := &customMetricsTriggerPayload{
			Enabled:            true,
			ExposedMetricsPath: custom.Path.ValueString(),
			ExposedMetricsPort: int(custom.Port.ValueInt64()),
			Metrics:            []customMetricPayload{},
		}

		for _, m := range metrics {
			trigger.Metrics = append(trigger.Metrics, customMetricPayload{
				MetricName:     m.Name.ValueString(),
				MetricType:     m.Type.ValueString(),
				ThresholdValue: m.Threshold.ValueFloat64(),
			})
		}

		h.UserMetrics = trigger
	}

	return &autoscalingPayload{Horizontal: h}, diags
}

func percentageTrigger(threshold types.Int64) *percentageTriggerPayload {
	if t := knownIntPointer(threshold); t != nil {
		return &percentageTriggerPayload{Enabled: true, ThresholdPercentage: *t}
	}

	return &percentageTriggerPayload{Enabled: false, ThresholdPercentage: disabledPercentageThreshold}
}

// autoscalingValue converts reported autoscaling into the block. Autoscaling
// that is off, and triggers that are off, read as null.
func autoscalingValue(ctx context.Context, reported *autoscalingPayload) (types.Object, diag.Diagnostics) {
	if reported == nil || !reported.Horizontal.Enabled {
		return types.ObjectNull(autoscalingAttrTypes), nil
	}

	var diags diag.Diagnostics
	h := reported.Horizontal

	model := autoscalingModel{
		MinReplicas:                types.Int64Value(int64(h.MinReplicas)),
		MaxReplicas:                types.Int64Value(int64(h.MaxReplicas)),
		CPUThresholdPercentage:     types.Int64Null(),
		MemoryThresholdPercentage:  types.Int64Null(),
		RequestsPerSecondThreshold: types.Int64Null(),
		CustomMetrics:              types.ObjectNull(customMetricsAttrTypes),
	}

	if h.CPU != nil && h.CPU.Enabled {
		model.CPUThresholdPercentage = types.Int64Value(int64(h.CPU.ThresholdPercentage))
	}

	if h.Memory != nil && h.Memory.Enabled {
		model.MemoryThresholdPercentage = types.Int64Value(int64(h.Memory.ThresholdPercentage))
	}

	if h.RPS != nil && h.RPS.Enabled {
		model.RequestsPerSecondThreshold = types.Int64Value(int64(h.RPS.ThresholdValue))
	}

	if u := h.UserMetrics; u != nil && u.Enabled {
		metrics := make([]customMetricModel, 0, len(u.Metrics))

		for _, m := range u.Metrics {
			metrics = append(metrics, customMetricModel{
				Name:      types.StringValue(m.MetricName),
				Type:      types.StringValue(m.MetricType),
				Threshold: types.Float64Value(m.ThresholdValue),
			})
		}

		list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: customMetricAttrTypes}, metrics)
		diags.Append(d...)

		custom, d := types.ObjectValue(customMetricsAttrTypes, map[string]attr.Value{
			"path":    types.StringValue(u.ExposedMetricsPath),
			"port":    types.Int64Value(int64(u.ExposedMetricsPort)),
			"metrics": list,
		})
		diags.Append(d...)
		model.CustomMetrics = custom
	}

	value, d := types.ObjectValueFrom(ctx, autoscalingAttrTypes, model)
	diags.Append(d...)

	return value, diags
}
