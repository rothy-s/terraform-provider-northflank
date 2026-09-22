# terraform-provider-northflank

A Terraform provider for the [Northflank](https://northflank.com) platform,
built on
[terraform-plugin-framework](https://github.com/hashicorp/terraform-plugin-framework).

It wraps the [Northflank API](https://northflank.com/docs/v1/api/introduction)
through a client generated from Northflank's own OpenAPI document.

## Status

Early. The generated client covers the whole API; the provider exposes the
first resource and data source on top of it. Behaviour is covered by unit
tests, tests against a fake API server and an offline plan, but not yet by
acceptance tests against the live API.

| Resource             | Update                                    |
| -------------------- | ----------------------------------------- |
| `northflank_project` | `description`, `color`; others replace    |

| Data source          | Lookup keys      |
| -------------------- | ---------------- |
| `northflank_project` | `id`, or `name`  |

A few things worth knowing about projects:

- **Renaming replaces the project.** Northflank derives the project `id` from
  its name and cannot rename one.
- **Destroying a non-empty project fails** unless `delete_child_objects = true`
  was applied first, in which case the project's services, jobs and addons are
  deleted with it.
- **`color` drift is not detected.** The API accepts a color but does not
  report it when reading a project.

## Usage

```hcl
terraform {
  required_providers {
    northflank = {
      source  = "srs/northflank"
      version = "0.1.0"
    }
  }
}

provider "northflank" {
  token = var.northflank_token # or the NORTHFLANK_API_TOKEN environment variable
}

resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}
```

### Authentication

The provider authenticates with a Northflank API token, created in the
dashboard under team settings, API, Tokens. A token is created from an API role
that decides what it may do, and it belongs to one team: every request acts on
that team.

| Attribute  | Environment variable   | Default                      |
| ---------- | ---------------------- | ---------------------------- |
| `token`    | `NORTHFLANK_API_TOKEN` | none, required               |
| `endpoint` | `NORTHFLANK_ENDPOINT`  | `https://api.northflank.com` |

`endpoint` is the base URL without the `/v1` suffix, since the generated
request paths already include it.

## Documentation

Per-resource reference lives in [`docs/`](docs/), generated from the provider
schemas. That is also what the Terraform Registry renders. Runnable
configuration is in [`examples/`](examples/).

The registry's overview page comes from [`templates/index.md.tmpl`](templates/index.md.tmpl)
rather than from a schema description, so prose for that page belongs there.
Everything else is generated from schemas plus `examples/`. `make docs` rebuilds
the tree and CI fails if it is stale.

## Development

| Package | Purpose |
| --- | --- |
| `internal/northflank` | generated API client, plus bearer auth and error helpers |
| `internal/apierr` | error decoding and retry of transient failures |
| `internal/provider` | the Terraform provider, resources and data sources |
| `internal/tools/normalizespec` | repairs the upstream spec so codegen accepts it |

```bash
make fetch-spec   # refresh the vendored OpenAPI document
make generate     # normalize the spec, then regenerate the API client
make docs         # regenerate docs/ from the provider schemas
make test         # unit tests
make smoke        # offline end-to-end plan through real Terraform
make check        # lint + test + smoke, i.e. what CI runs
make build        # build the provider binary
make install      # install into the local plugin mirror
```

Generated artifacts (`api/openapi.normalized.json`,
`internal/northflank/client.gen.go` and `docs/`) are committed, so a checkout
builds without running codegen. CI fails if they are stale.

### Where the spec comes from

Northflank serves its OpenAPI document from the API itself, at
`https://api.northflank.com/v1/swagger-json`, though the docs do not link to
it. `make fetch-spec` downloads it to `api/openapi.json`, which stays
byte-identical to upstream so refreshing it is a clean overwrite.

### Why the spec is normalized before codegen

The document claims to be OpenAPI 3.0.1 but mixes in Swagger 2.0 and OpenAPI
3.1 idioms that `oapi-codegen` rejects. `internal/tools/normalizespec` repairs
it:

- **Operations are references.** Every operation is a bare `$ref` into
  `components.pathItems`, which 3.0 allows for neither. They are inlined.
- **Responses use the Swagger 2.0 shape**, with `schema` directly on the
  response. It is moved under `content["application/json"]`.
- **Parameters are duplicated into their own schema.** Each parameter's
  `schema` repeats `name`, `in` and a boolean `required`, which is not valid in
  a schema. Those are stripped, along with the Swagger 2.0 schema keywords
  repeated on the parameter itself.
- **Parameter `examples` are arrays** (the 3.1 schema form) where 3.0 expects a
  map. The first entry becomes `example`.
- **Some operations declare path parameters their route lacks.** Those
  parameters are dropped.
- **Union request bodies lose their JSON encoding.** An inline `oneOf` or
  `anyOf` request body becomes `type XJSONRequestBody XJSONBody` in
  oapi-codegen, a defined type that drops the union's `MarshalJSON`, so the
  generated client would send `{}`. Those schemas move into
  `components.schemas`, which turns the request body type into an alias.
- **File maps use a placeholder key.** Objects whose only property is
  `/dir/fileName` are really maps keyed by path, and become
  `additionalProperties`.
- **Team-scoped routes are duplicated.** Almost every `/v1/...` route has a
  `/v1/teams/{teamId}/...` twin for org-scoped tokens, sharing one
  operationId. The twins are dropped, since a team API token addresses its
  team through the plain route. This also halves the generated client, to
  about 16 MB. Routes that exist only under `/v1/teams/{teamId}` are kept.

Generated per-operation response wrappers carry an `HTTPResponse` suffix, as in
the Convex provider, so they cannot collide with schema names.

### The smoke test

`make smoke` builds the provider, points Terraform at the binary with a
`dev_overrides` block, and plans [`test/smoke`](test/smoke) against it. That
runs the actual plugin protocol, so schemas, plan modifiers, validators and
attribute sensitivity are exercised the way Terraform exercises them.

It needs no credentials and no network, and applies nothing. It plans a *copy*
of the directory, so state left behind by a stray apply cannot turn the check
into a live refresh. The script derives the list of registered resources and
data sources from `provider.go` and fails if any is missing from the plan, so
every new resource must be added to `test/smoke/main.tf`.

If `terraform` is not installed the script exits 0 with a message, so it does
not break a Go-only checkout.

### Running against a local build

The source address is `srs/northflank`. Until a release is published it
resolves only to a local build.

**Local plugin mirror.** `make install` copies the binary into Terraform's
implied local mirror under the deliberately unresolvable address
`local/srs/northflank`, so a config pinning it can never silently fall back to
a published release. Re-run `make install` after every change, and delete
`.terraform.lock.hcl` when the checksum no longer matches.

**`dev_overrides`.** Points the address straight at a build directory, with no
copy, no lock file and no `terraform init`:

```hcl
# ~/.terraformrc
provider_installation {
  dev_overrides {
    "srs/northflank" = "/path/to/terraform-provider-northflank"
  }
  direct {}
}
```

### Cutting a release

1. **Create a GPG signing key** and add the public key to your registry
   namespace at registry.terraform.io.
2. **Add repository secrets** in GitHub: `GPG_PRIVATE_KEY` (ASCII-armored) and
   `PASSPHRASE`.
3. **Publish the provider** on registry.terraform.io by selecting this
   repository.
4. **Update `CHANGELOG.md`** and bump `VERSION` in the `Makefile`.
5. **Tag and push:** `git tag v0.1.0 && git push origin v0.1.0`. The
   [release workflow](.github/workflows/release.yml) builds every platform with
   GoReleaser and signs the checksums; the registry picks it up from the tag.

## License

Copyright 2026 Sten Roger Sandvik <stenrs@gmail.com>.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
