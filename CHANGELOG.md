# Changelog

Notable changes, newest first. Entries accumulate under **Unreleased** and get
a version heading and date when a release is tagged.

Grouped as Added / Changed / Fixed / Removed, following
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html), with the usual 0.x
caveat that minor releases may still break.

## Unreleased

### Added

- `northflank_project` resource: creates a project in a Northflank region or
  on a BYOC cluster. `description` and `color` update in place; `name`,
  `region` and `cluster_id` force replacement. `delete_child_objects` opts in
  to deleting the project's contents on destroy. Supports import by ID.
- `northflank_project` data source: looks a project up by `id` or `name`.
- `northflank_secret_group` resource: manages a project secret group's
  variables, priority, injection scope, permission type, tags and
  restrictions. Every update sends the complete variable map, and the read-back
  warns if Northflank kept a variable that was removed. Supports import as
  `<project_id>/<secret_group_id>`.
- `northflank_secret_group` data source: looks a secret group up by `id` or
  `name` within a project, including its decrypted variables.
- `northflank_job` resource: a cron or manual job running an external image
  or the output of a build service, with command and entrypoint overrides,
  retry and deadline settings, and a runtime environment. Created with
  `POST /v1/projects/{projectId}/jobs` and updated with `PUT` on the same path,
  which, unlike PATCH, can change the deployment source. Supports import as
  `<project_id>/<job_id>`.
- `northflank_job` data source: looks a job up by `id` or `name` within a
  project.
- Provider scaffold: `token` and `endpoint` configuration, also settable with
  `NORTHFLANK_API_TOKEN` and `NORTHFLANK_ENDPOINT`.
- API client generated with oapi-codegen from Northflank's OpenAPI document,
  vendored from `https://api.northflank.com/v1/swagger-json` and repaired by
  `internal/tools/normalizespec` first.
- Unit tests, an offline smoke plan, CI, and a GoReleaser setup for the
  Terraform Registry.
