package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func int64Set(values ...int64) types.Set {
	elems := make([]attr.Value, 0, len(values))

	for _, v := range values {
		elems = append(elems, types.Int64Value(v))
	}

	return types.SetValueMust(types.Int64Type, elems)
}

func dailyDumpPlan() addonBackupScheduleModel {
	return addonBackupScheduleModel{
		ProjectID:           types.StringValue("x-connect-lab"),
		AddonID:             types.StringValue("db"),
		BackupType:          types.StringValue("dump"),
		Interval:            types.StringValue("daily"),
		Minutes:             int64Set(0),
		Hours:               int64Set(3),
		Days:                types.SetNull(types.Int64Type),
		RetentionDays:       types.Int64Value(14),
		CompressionType:     types.StringNull(),
		CustomDestinationID: types.StringNull(),
	}
}

func TestBackupSchedulePayload(t *testing.T) {
	ctx := t.Context()

	plan := dailyDumpPlan()
	plan.Minutes = int64Set(30, 0)
	plan.CompressionType = types.StringValue("zstd")

	payload, diags := backupSchedulePayloadFrom(ctx, plan)

	if diags.HasError() {
		t.Fatal(diags)
	}

	body, err := json.Marshal(payload)

	if err != nil {
		t.Fatal(err)
	}

	want := `{"scheduling":{"interval":"daily","minute":[0,30],"hour":[3]},"backupType":"dump","retentionTime":14,"compressionType":"zstd"}`

	if string(body) != want {
		t.Errorf("payload = %s\nwant      %s", body, want)
	}

	roundTrip(t, body, &northflank.PostProjectsAddonsAddonidBackupSchedulesJSONRequestBody{})
	validateRequestBody(t, "/v1/projects/{projectId}/addons/{addonId}/backup-schedules", http.MethodPost, body)
}

func TestValidateBackupSchedule(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*addonBackupScheduleModel)
		ok   bool
	}{
		"daily with hours":        {func(*addonBackupScheduleModel) {}, true},
		"daily without hours":     {func(m *addonBackupScheduleModel) { m.Hours = types.SetNull(types.Int64Type) }, false},
		"daily with days":         {func(m *addonBackupScheduleModel) { m.Days = int64Set(6) }, false},
		"weekly without days":     {func(m *addonBackupScheduleModel) { m.Interval = types.StringValue("weekly") }, false},
		"weekly with hours, days": {func(m *addonBackupScheduleModel) { m.Interval = types.StringValue("weekly"); m.Days = int64Set(6) }, true},
		"hourly with hours":       {func(m *addonBackupScheduleModel) { m.Interval = types.StringValue("hourly") }, false},
		"hourly": {func(m *addonBackupScheduleModel) {
			m.Interval = types.StringValue("hourly")
			m.Hours = types.SetNull(types.Int64Type)
		}, true},
		"unknown hours": {func(m *addonBackupScheduleModel) { m.Hours = types.SetUnknown(types.Int64Type) }, true},
		"snapshot with compression": {func(m *addonBackupScheduleModel) {
			m.BackupType = types.StringValue("snapshot")
			m.CompressionType = types.StringValue("gz")
		}, false},
	} {
		m := dailyDumpPlan()
		tc.edit(&m)

		if diags := validateBackupSchedule(m); diags.HasError() == tc.ok {
			t.Errorf("%s: diagnostics = %v, want ok = %v", name, diags, tc.ok)
		}
	}
}

func TestGetBackupSchedule(t *testing.T) {
	ctx := t.Context()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/projects/p/addons/db/backup-schedules", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"schedules":[
			{"id":"other","backupType":"snapshot","scheduling":{"interval":"hourly","minute":[15]},"retentionTime":1},
			{"id":"s1","backupType":"dump","backupCompressionType":"gz",
			 "scheduling":{"interval":"daily","minute":[0],"hour":[3]},"retentionTime":14}]}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := northflank.NewAuthenticated(server.URL, "tok")

	if err != nil {
		t.Fatal(err)
	}

	read, err := getBackupSchedule(ctx, client, "p", "db", "s1")

	if err != nil {
		t.Fatal(err)
	}

	// Planned without a compression type: Northflank's reported default must
	// not show up as drift.
	model := dailyDumpPlan()

	if diags := applyBackupSchedule(ctx, &model, read, true); diags.HasError() {
		t.Fatal(diags)
	}

	if want := dailyDumpPlan(); !model.CompressionType.IsNull() || !model.Hours.Equal(want.Hours) ||
		!model.Days.IsNull() || model.RetentionDays.ValueInt64() != 14 || model.ID.ValueString() != "s1" {
		t.Errorf("model = %+v", model)
	}

	// Imported: everything comes from the API.
	imported := addonBackupScheduleModel{ProjectID: types.StringValue("p"), AddonID: types.StringValue("db")}
	applyBackupSchedule(ctx, &imported, read, false)

	if imported.CompressionType.ValueString() != "gz" || imported.Interval.ValueString() != "daily" {
		t.Errorf("imported = %+v", imported)
	}

	if _, err := getBackupSchedule(ctx, client, "p", "db", "gone"); !isNotFound(err) {
		t.Errorf("getBackupSchedule(gone) = %v, want a 404", err)
	}
}
