package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func pathRoot(name string) path.Path { return path.Root(name) }

func fakeSecretGroupAPI(t *testing.T) *secretGroupDataSource {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/projects/my-project/secrets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("cursor") == "" {
			_, _ = fmt.Fprint(w, `{"data":{"secrets":[
				{"id":"first","name":"First","priority":10,"projectId":"my-project","secretType":"environment","type":"secret","tags":[],"restrictions":{}}
			]},"pagination":{"hasNextPage":true,"cursor":"next","count":1}}`)
			return
		}

		_, _ = fmt.Fprint(w, `{"data":{"secrets":[
			{"id":"app-secrets","name":"App Secrets","priority":10,"projectId":"my-project","secretType":"environment","type":"secret","tags":[],"restrictions":{}}
		]},"pagination":{"hasNextPage":false,"count":1}}`)
	})

	mux.HandleFunc("GET /v1/projects/my-project/secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Inherited addon secrets must never end up in state.
		if got := r.URL.Query().Get("show"); got != "this" {
			t.Errorf("show = %q, want this", got)
		}

		if r.PathValue("id") != "app-secrets" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":{"status":404,"message":"Secret not found"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{
			"id":"app-secrets","name":"App Secrets","projectId":"my-project","priority":20,
			"secretType":"environment-arguments","type":"secret","tags":["prod"],
			"restrictions":{"restricted":true,"nfObjects":[{"id":"api","type":"service"}],"tags":[],"stageIds":[]},
			"secrets":{"variables":{"DATABASE_URL":"postgres://x"}},
			"createdAt":"2024-05-01T00:00:00.000Z","updatedAt":"2024-05-02T00:00:00.000Z"
		}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := northflank.NewAuthenticated(server.URL, "tok")

	if err != nil {
		t.Fatal(err)
	}

	return &secretGroupDataSource{client: client}
}

func TestFindSecretGroupIDByName(t *testing.T) {
	d := fakeSecretGroupAPI(t)

	id, err := d.findSecretGroupIDByName(t.Context(), "my-project", "App Secrets")

	if err != nil {
		t.Fatalf("findSecretGroupIDByName() error = %v", err)
	}

	if id != "app-secrets" {
		t.Errorf("id = %q, want app-secrets", id)
	}

	if _, err := d.findSecretGroupIDByName(t.Context(), "my-project", "Nope"); err == nil || !strings.Contains(err.Error(), "no secret group named") {
		t.Errorf("error = %v, want not found", err)
	}
}

func TestGetSecretGroup(t *testing.T) {
	d := fakeSecretGroupAPI(t)
	ctx := t.Context()

	group, err := getSecretGroup(ctx, d.client, "my-project", "app-secrets")

	if err != nil {
		t.Fatalf("getSecretGroup() error = %v", err)
	}

	model, diags := secretGroupDataSourceModelFrom(ctx, group)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if model.Priority.ValueInt64() != 20 || model.UpdatedAt.ValueString() != "2024-05-02T00:00:00Z" {
		t.Errorf("priority %v, updated_at %v", model.Priority, model.UpdatedAt)
	}

	variables := map[string]string{}
	model.Variables.ElementsAs(ctx, &variables, false)

	if variables["DATABASE_URL"] != "postgres://x" {
		t.Errorf("variables = %v", variables)
	}

	var r secretGroupRestrictionsModel
	model.Restrictions.As(ctx, &r, objectAsOptions)

	if !r.Restricted.ValueBool() || len(r.Resources.Elements()) != 1 || !r.Tags.IsNull() {
		t.Errorf("restrictions = %+v", r)
	}

	if _, err := getSecretGroup(ctx, d.client, "my-project", "gone"); !isNotFound(err) {
		t.Errorf("error = %v, want a 404", err)
	}
}
