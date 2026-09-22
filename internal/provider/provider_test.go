package provider

import (
	"testing"

	fwdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
)

func TestProviderSchema(t *testing.T) {
	ctx := t.Context()
	resp := &fwprovider.SchemaResponse{}
	New("test")().Schema(ctx, fwprovider.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema() diagnostics: %+v", resp.Diagnostics)
	}

	if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
		t.Fatalf("ValidateImplementation() diagnostics: %+v", diags)
	}
}

func TestProviderMetadata(t *testing.T) {
	resp := &fwprovider.MetadataResponse{}
	New("1.2.3")().Metadata(t.Context(), fwprovider.MetadataRequest{}, resp)

	if resp.TypeName != "northflank" {
		t.Errorf("TypeName = %q, want %q", resp.TypeName, "northflank")
	}

	if resp.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", resp.Version, "1.2.3")
	}
}

func TestProviderDataFrom(t *testing.T) {
	var diags diag.Diagnostics

	if _, ok := providerDataFrom(nil, &diags); ok || diags.HasError() {
		t.Errorf("providerDataFrom(nil) = ok %v, diags %v; want not ok and no error", ok, diags)
	}

	if _, ok := providerDataFrom("wrong", &diags); ok || !diags.HasError() {
		t.Errorf("providerDataFrom(string) = ok %v, diags %v; want not ok and an error", ok, diags)
	}

	diags = nil
	want := &providerData{}

	if got, ok := providerDataFrom(want, &diags); !ok || got != want || diags.HasError() {
		t.Errorf("providerDataFrom(*providerData) = %v, %v, %v; want the same pointer", got, ok, diags)
	}
}

// Every registered resource must declare a schema the framework accepts, and
// be registered only once.
func TestProviderResourceSchemas(t *testing.T) {
	ctx := t.Context()
	seen := map[string]bool{}

	for _, newResource := range New("test")().Resources(ctx) {
		res := newResource()
		metadata := &fwresource.MetadataResponse{}
		res.Metadata(ctx, fwresource.MetadataRequest{ProviderTypeName: "northflank"}, metadata)

		if seen[metadata.TypeName] {
			t.Errorf("resource %q is registered more than once", metadata.TypeName)
		}

		seen[metadata.TypeName] = true

		t.Run(metadata.TypeName, func(t *testing.T) {
			resp := &fwresource.SchemaResponse{}
			res.Schema(ctx, fwresource.SchemaRequest{}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("Schema() diagnostics: %+v", resp.Diagnostics)
			}

			if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
				t.Fatalf("ValidateImplementation() diagnostics: %+v", diags)
			}
		})
	}
}

func TestProviderRegistersProject(t *testing.T) {
	ctx := t.Context()
	p := New("test")()

	resources := map[string]bool{}

	for _, newResource := range p.Resources(ctx) {
		metadata := &fwresource.MetadataResponse{}
		newResource().Metadata(ctx, fwresource.MetadataRequest{ProviderTypeName: "northflank"}, metadata)
		resources[metadata.TypeName] = true
	}

	dataSources := map[string]bool{}

	for _, newDataSource := range p.DataSources(ctx) {
		metadata := &fwdatasource.MetadataResponse{}
		newDataSource().Metadata(ctx, fwdatasource.MetadataRequest{ProviderTypeName: "northflank"}, metadata)
		dataSources[metadata.TypeName] = true
	}

	if !resources["northflank_project"] {
		t.Error("resource northflank_project is not registered")
	}

	if !dataSources["northflank_project"] {
		t.Error("data source northflank_project is not registered")
	}
}

// Every registered data source must declare a schema the framework accepts.
func TestProviderDataSourceSchemas(t *testing.T) {
	ctx := t.Context()

	for _, newDataSource := range New("test")().DataSources(ctx) {
		ds := newDataSource()
		metadata := &fwdatasource.MetadataResponse{}
		ds.Metadata(ctx, fwdatasource.MetadataRequest{ProviderTypeName: "northflank"}, metadata)

		t.Run(metadata.TypeName, func(t *testing.T) {
			resp := &fwdatasource.SchemaResponse{}
			ds.Schema(ctx, fwdatasource.SchemaRequest{}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("Schema() diagnostics: %+v", resp.Diagnostics)
			}

			if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
				t.Fatalf("ValidateImplementation() diagnostics: %+v", diags)
			}
		})
	}
}
