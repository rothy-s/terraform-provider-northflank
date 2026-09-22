package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// addonResponse is a get-addon response in the documented shape.
func addonResponse(status, version string) string {
	return fmt.Sprintf(`{"data":{"id":"db","uid":"68f0aa1b9c4e2d7f11a3b402","appId":"/team/p/db","name":"DB",
		"description":"Main database","tags":["prod"],"status":%q,"createdAt":"2024-05-01T00:00:00Z",
		"cluster":{"id":"c","name":"c"},
		"spec":{"type":"postgresql","pendingActions":[],"config":{
			"versionTag":%q,"lifecycleStatus":"active",
			"deployment":{"planId":"nf-compute-20","region":"europe-west","replicas":2,"storageClass":"nvme","storageSize":8192},
			"networking":{"externalAccessEnabled":false,"tlsEnabled":true,"vpcAccessible":false}}}}}`, status, version)
}

func TestAddonPayloadsMatchSpec(t *testing.T) {
	dbName := "app"
	create, _ := json.Marshal(addonCreatePayload{
		Name: "DB", Tags: []string{"prod"}, Type: "postgresql", Version: "16",
		Billing:    addonBillingPayload{DeploymentPlan: "nf-compute-20", Replicas: 2, Storage: 8192},
		TLSEnabled: true, CustomCredentials: &addonCredentialPayload{DBName: dbName},
	})

	plan, state := addonModel("16.4.0", 2, 8192), addonModel("16.2.0", 1, 4096)
	var diags = &plan.diags
	patch, _ := json.Marshal(addonPatchFrom(t.Context(), &plan.model, &state.model, diags))

	t.Run("create", func(t *testing.T) { roundTrip(t, create, &northflank.PostProjectsAddonsJSONRequestBody{}) })
	t.Run("update", func(t *testing.T) { roundTrip(t, patch, &northflank.PatchProjectsAddonsAddonidJSONRequestBody{}) })
}

type addonModelWithDiags struct {
	model addonResourceModel
	diags diag.Diagnostics
}

func addonModel(version string, replicas, storage int64) addonModelWithDiags {
	return addonModelWithDiags{model: addonResourceModel{
		Description:    types.StringValue("Main database"),
		Tags:           types.SetValueMust(types.StringType, stringValues([]string{"prod"})),
		Version:        types.StringValue(version),
		DeploymentPlan: types.StringValue("nf-compute-20"),
		Replicas:       types.Int64Value(replicas),
		StorageSize:    types.Int64Value(storage),
		StorageClass:   types.StringValue("nvme"),
		TLSEnabled:     types.BoolValue(true),
		ExternalAccess: types.BoolValue(false),
		VpcAccessible:  types.BoolValue(false),
	}}
}

// An update must only carry what changed, to leave a running database alone
// as far as possible.
func TestAddonPatchOnlyChanges(t *testing.T) {
	ctx := t.Context()

	same := addonModel("16", 1, 4096)
	var d diag.Diagnostics

	if patch := addonPatchFrom(ctx, &same.model, &same.model, &d); !patch.empty() {
		raw, _ := json.Marshal(patch)
		t.Errorf("patch for an unchanged addon = %s, want nothing", raw)
	}

	plan, state := addonModel("16", 3, 4096), addonModel("16", 1, 4096)
	plan.model.ExternalAccess = types.BoolValue(true)
	patch := addonPatchFrom(ctx, &plan.model, &state.model, &d)
	raw, _ := json.Marshal(patch)

	if string(raw) != `{"billing":{"replicas":3},"externalAccessEnabled":true}` {
		t.Errorf("patch = %s", raw)
	}
}

func TestAddonVersionMatches(t *testing.T) {
	for _, tt := range []struct {
		configured, running string
		want                bool
	}{
		{"16.4.0", "16.4.0", true},
		{"16", "16.4.0", true},
		{"16.4", "16.4.0", true},
		{"16", "160.1.0", false},
		{"latest", "16.4.0", true},
		{"15", "16.4.0", false},
		{"16", "", true},
		{"", "16.4.0", false},
	} {
		if got := addonVersionMatches(tt.configured, tt.running); got != tt.want {
			t.Errorf("addonVersionMatches(%q, %q) = %v, want %v", tt.configured, tt.running, got, tt.want)
		}
	}
}

func TestApplyAddon(t *testing.T) {
	ctx := t.Context()
	addon, err := addonFromBody([]byte(addonResponse("running", "16.4.0")))

	if err != nil {
		t.Fatal(err)
	}

	model := addonResourceModel{Version: types.StringValue("16")}

	if diags := applyAddon(ctx, &model, addon); diags.HasError() {
		t.Fatal(diags)
	}

	checks := map[string][2]any{
		"version":         {model.Version.ValueString(), "16"},
		"running_version": {model.RunningVersion.ValueString(), "16.4.0"},
		"replicas":        {model.Replicas.ValueInt64(), int64(2)},
		"storage_size":    {model.StorageSize.ValueInt64(), int64(8192)},
		"storage_class":   {model.StorageClass.ValueString(), "nvme"},
		"region":          {model.Region.ValueString(), "europe-west"},
		"tls_enabled":     {model.TLSEnabled.ValueBool(), true},
		"type":            {model.Type.ValueString(), "postgresql"},
	}

	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}

	// Upgraded outside Terraform: the running version shows as drift.
	model.Version = types.StringValue("15")
	applyAddon(ctx, &model, addon)

	if model.Version.ValueString() != "16.4.0" {
		t.Errorf("version = %v, want the running version recorded", model.Version)
	}
}

// fakeAddonAPI serves an addon that runs after two polls, is then deleted
// after two more, and has credentials once running.
func fakeAddonAPI(t *testing.T, finalStatus string) *northflank.ClientWithResponses {
	t.Helper()

	var polls, deletePolls atomic.Int32
	var deleted atomic.Bool

	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/projects/p/addons/db", func(w http.ResponseWriter, r *http.Request) {
		if deleted.Load() {
			if deletePolls.Add(1) > 2 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"error":{"message":"not found"}}`)
				return
			}

			_, _ = fmt.Fprint(w, addonResponse("deleting", "16.4.0"))
			return
		}

		status := "allocating"

		if polls.Add(1) > 2 {
			status = finalStatus
		}

		_, _ = fmt.Fprint(w, addonResponse(status, "16.4.0"))
	})

	mux.HandleFunc("DELETE /v1/projects/p/addons/db", func(w http.ResponseWriter, r *http.Request) {
		deleted.Store(true)
		_, _ = fmt.Fprint(w, `{"data":{}}`)
	})

	mux.HandleFunc("GET /v1/projects/p/addons/db/credentials", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"secrets":{"username":"u","password":"p","port":5432},
			"envs":{"POSTGRES_URI":"postgresql://u:p@db:5432/app","TLS":null}}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := northflank.NewAuthenticated(server.URL, "tok")

	if err != nil {
		t.Fatal(err)
	}

	return client
}

func shortAddonWaits(t *testing.T) {
	t.Helper()

	interval, ready, gone := addonPollInterval, addonReadyTimeout, addonDeleteTimeout
	addonPollInterval, addonReadyTimeout, addonDeleteTimeout = time.Millisecond, 5*time.Second, 5*time.Second

	t.Cleanup(func() {
		addonPollInterval, addonReadyTimeout, addonDeleteTimeout = interval, ready, gone
	})
}

func TestWaitForAddon(t *testing.T) {
	shortAddonWaits(t)
	ctx := t.Context()

	t.Run("becomes ready", func(t *testing.T) {
		client := fakeAddonAPI(t, "running")
		addon, err := waitForAddon(ctx, client, "p", "db")

		if err != nil || addon.Status != "running" {
			t.Fatalf("waitForAddon() = %v, %v", addon, err)
		}

		creds, err := getAddonCredentials(ctx, client, "p", "db")

		if err != nil {
			t.Fatal(err)
		}

		// Non-string values are encoded, and nulls left out.
		if creds.Secrets["port"] != "5432" || creds.Envs["POSTGRES_URI"] == "" {
			t.Errorf("credentials = %+v", creds)
		}

		if _, ok := creds.Envs["TLS"]; ok {
			t.Error("a null credential was recorded")
		}

		addonDeleteTimeout = 20 * time.Millisecond

		if err := waitForAddonDeleted(ctx, client, "p", "db"); err == nil {
			t.Error("waitForAddonDeleted() succeeded for an addon that was never deleted")
		}
	})

	t.Run("fails", func(t *testing.T) {
		addonDeleteTimeout = 5 * time.Second

		_, err := waitForAddon(ctx, fakeAddonAPI(t, "errorAllocating"), "p", "db")

		if err == nil || !strings.Contains(err.Error(), "errorAllocating") {
			t.Errorf("waitForAddon() error = %v, want the failed state reported", err)
		}
	})

	t.Run("times out", func(t *testing.T) {
		addonReadyTimeout = 20 * time.Millisecond
		_, err := waitForAddon(ctx, fakeAddonAPI(t, "allocating"), "p", "db")

		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("waitForAddon() error = %v, want a timeout", err)
		}
	})
}

func TestAddonDeleteWaitsUntilGone(t *testing.T) {
	shortAddonWaits(t)
	ctx := t.Context()
	client := fakeAddonAPI(t, "running")

	deleted, err := client.DeleteProjectsAddonsAddonidWithResponse(ctx, "p", "db")

	if err != nil || northflank.CheckResponse(deleted) != nil {
		t.Fatalf("delete = %v", err)
	}

	if err := waitForAddonDeleted(ctx, client, "p", "db"); err != nil {
		t.Errorf("waitForAddonDeleted() = %v", err)
	}
}

func TestAddonDataSourceModel(t *testing.T) {
	ctx := t.Context()
	addon, _ := addonFromBody([]byte(addonResponse("running", "16.4.0")))

	model, diags := addonDataSourceModelFrom(ctx, "p", addon, &addonCredentials{
		Secrets: map[string]string{"password": "p"},
		Envs:    map[string]string{},
	})

	if diags.HasError() {
		t.Fatal(diags)
	}

	if model.Version.ValueString() != "16.4.0" || len(model.Secrets.Elements()) != 1 || !model.ConnectionDetails.IsNull() {
		t.Errorf("model = %+v", model)
	}

	model, _ = addonDataSourceModelFrom(ctx, "p", addon, nil)

	if !model.Secrets.IsNull() {
		t.Errorf("secrets = %v, want null without credentials", model.Secrets)
	}
}
