package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func TestVolumePayloadsMatchSpec(t *testing.T) {
	sub := "postgres"
	storageClass := "nvme"

	create, _ := json.Marshal(volumeCreatePayload{
		Name:            "Data",
		Spec:            volumeSpecPayload{AccessMode: "ReadWriteOnce", StorageClassName: &storageClass, StorageSize: 10240},
		Mounts:          []volumeMountPayload{{ContainerMountPath: "/var/lib/postgresql", VolumeMountPath: &sub}},
		AttachedObjects: []volumeAttachmentPayload{{ID: "db", Type: "service"}},
	})
	update, _ := json.Marshal(volumeUpdatePayload{
		Spec:   volumeSizePayload{StorageSize: 20480},
		Mounts: []volumeMountPayload{{ContainerMountPath: "/data"}},
	})
	attach, _ := json.Marshal(volumeAttachRequest{NfObject: volumeAttachmentPayload{ID: "db", Type: "service"}})

	t.Run("create", func(t *testing.T) { roundTrip(t, create, &northflank.PostProjectsVolumesJSONRequestBody{}) })
	t.Run("update", func(t *testing.T) { roundTrip(t, update, &northflank.PostProjectsVolumesVolumeidJSONRequestBody{}) })
	t.Run("attach", func(t *testing.T) {
		roundTrip(t, attach, &northflank.PostProjectsVolumesVolumeidAttachJSONRequestBody{})
	})
	t.Run("detach", func(t *testing.T) {
		roundTrip(t, attach, &northflank.PostProjectsVolumesVolumeidDetachJSONRequestBody{})
	})
}

func TestAttachmentChanges(t *testing.T) {
	a := volumeAttachmentPayload{ID: "api", Type: "service"}
	b := volumeAttachmentPayload{ID: "worker", Type: "service"}
	c := volumeAttachmentPayload{ID: "backup", Type: "job"}

	detach, attach := attachmentChanges([]volumeAttachmentPayload{a, b}, []volumeAttachmentPayload{b, c})

	if !reflect.DeepEqual(detach, []volumeAttachmentPayload{a}) || !reflect.DeepEqual(attach, []volumeAttachmentPayload{c}) {
		t.Errorf("detach %v, attach %v", detach, attach)
	}

	if detach, attach := attachmentChanges([]volumeAttachmentPayload{a}, []volumeAttachmentPayload{a}); detach != nil || attach != nil {
		t.Errorf("no change produced detach %v, attach %v", detach, attach)
	}
}

// setAttachment must send the documented body to the right endpoint.
func TestSetAttachment(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)

	mux := http.NewServeMux()

	for _, action := range []string{"attach", "detach"} {
		mux.HandleFunc("POST /v1/projects/p/volumes/data/"+action, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)

			mu.Lock()
			calls = append(calls, action+" "+string(body))
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":{}}`)
		})
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, _ := northflank.NewAuthenticated(server.URL, "tok")
	r := &volumeResource{client: client}
	ctx := t.Context()

	object := volumeAttachmentPayload{ID: "db", Type: "service"}

	if err := r.setAttachment(ctx, "p", "data", object, false); err != nil {
		t.Fatal(err)
	}

	if err := r.setAttachment(ctx, "p", "data", object, true); err != nil {
		t.Fatal(err)
	}

	want := []string{
		`detach {"nfObject":{"id":"db","type":"service"}}`,
		`attach {"nfObject":{"id":"db","type":"service"}}`,
	}

	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestApplyVolumeKeepsMounts(t *testing.T) {
	ctx := t.Context()
	nvme := "nvme"

	mounts := types.ListValueMust(types.ObjectType{AttrTypes: volumeMountAttrTypes}, []attr.Value{
		types.ObjectValueMust(volumeMountAttrTypes, map[string]attr.Value{
			"container_path": types.StringValue("/data"),
			"volume_path":    types.StringNull(),
		}),
	})

	model := volumeResourceModel{Mounts: mounts}

	diags := applyVolume(ctx, &model, &volumeView{
		ID: "data", Name: "Data", StorageSize: 10240, StorageClass: &nvme, AccessMode: "ReadWriteOnce",
		Status: "BOUND", Attachments: []volumeAttachmentPayload{{ID: "db", Type: "service"}},
	})

	if diags.HasError() {
		t.Fatal(diags)
	}

	if !model.Mounts.Equal(mounts) {
		t.Errorf("mounts = %v, want the configured mounts kept", model.Mounts)
	}

	if model.StorageClass.ValueString() != "nvme" || len(model.Attachments.Elements()) != 1 || !model.UID.IsNull() {
		t.Errorf("model = %+v", model)
	}
}

func TestVolumeMountValidators(t *testing.T) {
	ctx := t.Context()

	check := func(v noMatchValidator, value string) bool {
		resp := &validator.StringResponse{}
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("x"), ConfigValue: types.StringValue(value)}, resp)
		return !resp.Diagnostics.HasError()
	}

	colon := noMatchValidator{pattern: volumeMountColon, message: "must not contain ':'"}
	parent := noMatchValidator{pattern: volumeMountParent, message: "must not contain '..'"}

	if !check(colon, "/data") || check(colon, "/data:ro") {
		t.Error("container path validation is wrong")
	}

	if !check(parent, "pg/data") || check(parent, "../etc") {
		t.Error("volume path validation is wrong")
	}
}

func TestVolumeDataSourceLookups(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/projects/p/volumes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[
			{"id":"data","name":"Data","tags":[],"status":"BOUND","createdAt":"2024-05-01T00:00:00Z",
			 "updatedAt":"2024-05-01T00:00:00Z","spec":{"accessMode":"ReadWriteOnce","storageSize":10240}}
		],"pagination":{"hasNextPage":false,"count":1}}`)
	})

	mux.HandleFunc("GET /v1/projects/p/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "data" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"id":"data","uid":"68f0aa1b9c4e2d7f11a3b402","name":"Data","tags":[],
			"status":"BOUND","createdAt":"2024-05-01T00:00:00Z","updatedAt":"2024-05-01T00:00:00Z",
			"spec":{"accessMode":"ReadWriteOnce","storageClassName":"nvme","storageSize":10240},
			"attachedObjects":[{"id":"db","type":"service"}]}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, _ := northflank.NewAuthenticated(server.URL, "tok")
	ctx := t.Context()
	d := &volumeDataSource{client: client}

	id, err := d.findVolumeIDByName(ctx, "p", "Data")

	if err != nil || id != "data" {
		t.Fatalf("findVolumeIDByName() = %q, %v", id, err)
	}

	volume, err := getVolume(ctx, client, "p", id)

	if err != nil {
		t.Fatal(err)
	}

	model, diags := volumeDataSourceModelFrom(ctx, "p", volume)

	if diags.HasError() {
		t.Fatal(diags)
	}

	if model.StorageSize.ValueInt64() != 10240 || model.StorageClass.ValueString() != "nvme" ||
		len(model.Attachments.Elements()) != 1 || model.UID.ValueString() == "" {
		t.Errorf("model = %+v", model)
	}

	if _, err := getVolume(ctx, client, "p", "gone"); !isNotFound(err) {
		t.Errorf("error = %v, want a 404", err)
	}
}
