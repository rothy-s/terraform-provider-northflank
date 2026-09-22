package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// fakeProjectAPI serves two pages of projects and a single project by ID.
func fakeProjectAPI(t *testing.T) *projectDataSource {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("cursor") == "" {
			_, _ = fmt.Fprint(w, `{"data":{"projects":[
				{"id":"alpha","uid":"a","name":"Alpha"},
				{"id":"twin-1","uid":"t1","name":"Twin"}
			]},"pagination":{"hasNextPage":true,"cursor":"page-2","count":2}}`)
			return
		}

		_, _ = fmt.Fprint(w, `{"data":{"projects":[
			{"id":"beta","uid":"b","name":"Beta"},
			{"id":"twin-2","uid":"t2","name":"Twin"}
		]},"pagination":{"hasNextPage":false,"count":2}}`)
	})

	mux.HandleFunc("GET /v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "beta" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":{"status":404,"message":"Project not found"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{
			"id":"beta","uid":"68f0aa1b9c4e2d7f11a3b402","name":"Beta","description":"second",
			"createdAt":"2021-01-20T11:19:53.175Z",
			"cluster":{"id":"nf-europe-west","name":"nf-europe-west"},
			"deployment":{"region":"europe-west"},
			"services":[],"jobs":[],"addons":[]
		}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := northflank.NewAuthenticated(server.URL, "tok")

	if err != nil {
		t.Fatal(err)
	}

	return &projectDataSource{client: client}
}

func TestFindProjectIDByName(t *testing.T) {
	d := fakeProjectAPI(t)

	// On the second page, so this proves the cursor is followed.
	id, err := d.findProjectIDByName(t.Context(), "Beta")

	if err != nil {
		t.Fatalf("findProjectIDByName(Beta) error = %v", err)
	}

	if id != "beta" {
		t.Errorf("id = %q, want beta", id)
	}

	if _, err := d.findProjectIDByName(t.Context(), "Missing"); err == nil || !strings.Contains(err.Error(), "no project named") {
		t.Errorf("findProjectIDByName(Missing) error = %v, want a not-found error", err)
	}

	if _, err := d.findProjectIDByName(t.Context(), "Twin"); err == nil || !strings.Contains(err.Error(), "2 projects") {
		t.Errorf("findProjectIDByName(Twin) error = %v, want an ambiguity error", err)
	}
}

func TestGetProject(t *testing.T) {
	d := fakeProjectAPI(t)

	project, err := getProject(t.Context(), d.client, "beta")

	if err != nil {
		t.Fatalf("getProject(beta) error = %v", err)
	}

	model := projectDataSourceModelFrom(project)

	checks := map[string][2]string{
		"id":          {model.ID.ValueString(), "beta"},
		"uid":         {model.UID.ValueString(), "68f0aa1b9c4e2d7f11a3b402"},
		"description": {model.Description.ValueString(), "second"},
		"region":      {model.Region.ValueString(), "europe-west"},
		"cluster_id":  {model.ClusterID.ValueString(), "nf-europe-west"},
		"created_at":  {model.CreatedAt.ValueString(), "2021-01-20T11:19:53Z"},
	}

	for attr, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", attr, c[0], c[1])
		}
	}

	if _, err := getProject(t.Context(), d.client, "gone"); !isNotFound(err) {
		t.Errorf("getProject(gone) error = %v, want a 404", err)
	}
}
