# Changelog

Notable changes, newest first. Entries accumulate under **Unreleased** and get
a version heading and date when a release is tagged.

Grouped as Added / Changed / Fixed / Removed, following
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html), with the usual 0.x
caveat that minor releases may still break.

## Unreleased

### Added

- `northflank_addon_backup_schedule` resource: scheduled `dump` or `snapshot`
  backups of an addon, hourly, daily or weekly, with a retention time.
  Northflank cannot change a schedule, so any change replaces it.
- `disable_nf_domain` on service ports, to stop routing the default `code.run`
  domain of a public HTTP port that has custom domains. The service data
  source reports it too.
- `stage_id` on addons, build, combined and deployment services and jobs, to
  put them in one of a project's environments. Changing it moves the
  resource; left unset, a resource stays where it is. Secret groups have no
  environment of their own (Northflank ignores a `stageId` sent for one); use
  `restrictions.stage_ids` to limit one to environments.
- `docker_secret_mounts` on secret groups, for build secrets read with
  `RUN --mount=type=secret`. Northflank does not report mounts back, so
  changes made outside Terraform are not detected.
- `cache_storage_size` on Dockerfile builds (build, combined services and
  jobs): the persistent disk for the buildkit layer cache, in MB. Builds that
  run in your own cluster need it for `use_cache`; without it Northflank
  rejects the service with "Internal cache storage is required when using
  internal cache".
- `architecture` (`arm` or `x86`) on build, combined and deployment services
  and jobs, to build arm images and run on arm nodes. Northflank defaults to
  `x86`. Northflank cannot change it after creation, so changing it replaces
  the resource. It does not report the setting for a job, so a job's value is
  not checked against Northflank.

### Fixed

- A deployment service built by a Northflank build service read its
  `deployment.build_service.id` back as `""`, so every plan wanted to change it
  and the apply failed with an inconsistent result. Northflank reports the ID
  as `nfObjectId` rather than the documented `id`; both are read now. The
  service data source had the same problem with `build_service_id`.
- Replacing a service or job failed with "Service already exists": Northflank
  removes a deleted service or job minutes after accepting the delete, and
  the name stays taken until then. Deleting now waits until it is gone, as
  it already did for addons.
- Domain verification failed with "HTTP 404: Could not verify TXT record" for
  a domain below the DNS zone, such as `lab.example.com` in `example.com`.
  `verification_record_name` was the name relative to the domain
  (`verify-abc`), so a DNS provider put the record at `verify-abc.example.com`.
  It is now the full name (`verify-abc.lab.example.com`). Verification also
  keeps retrying on that 404, which Northflank returns while the record is
  missing or not yet propagated, instead of failing at once. The same goes
  for subdomains ("Subdomain ... could not be verified"), including
  "Cloudflare proxy detected" while a proxied wildcard record still answers
  for a new, unproxied CNAME.
- Updating a service with a liveness or startup probe failed with
  "successThreshold is only allowed when 'type' is set to 'readinessProbe'":
  `success_threshold` defaulted to 1 on every probe. It is now sent only when
  set, rejected at plan time on anything but a `readinessProbe`, and a
  threshold Northflank reports for a probe that did not set one is not
  recorded.
- API errors now include the details Northflank sends with them, such as the
  field-level reasons behind "Request failed payload validation - see
  details", instead of dropping them.
- "Provider produced inconsistent result after apply" when Northflank leaves
  a configured value out of its response: a repository's `account_login` on
  jobs, build services and combined services, and a secret group's `type` and
  `secret_type`. The configured value is now kept when nothing is reported,
  while a different reported value still shows as drift.

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
- `northflank_job` can build from Git itself, like a combined service: set
  `repository`, `branch` and `build` (with optional `build_arguments`,
  `build_plan` and `disable_ci`) instead of `deployment.external_image` or
  `deployment.build_service`. The `deployment` block is now optional, and
  only needed for command and entrypoint overrides in that case. The
  `northflank_job` data source reports such a job's `repository_url` and
  `branch`.
- `run_on_source_change` on `northflank_job`: `never` (default), `always` or
  `cd-promote`, deciding whether a new build of the job's source also runs it.
- `storage_size` in the `dockerfile` and `buildpack` build blocks of jobs,
  build services and combined services: each build's ephemeral disk in MB.
- `northflank_deployment_service` resource: a long-running service running an
  external image or the output of a build service, with instance count,
  command and entrypoint overrides, ports, health checks and a runtime
  environment, all read back for drift detection. Created with
  `POST /v1/projects/{projectId}/services/deployment` and updated with `PUT` on
  the same path. Supports import as `<project_id>/<service_id>`.
- `northflank_build_service` resource: builds from a Git repository with a
  Dockerfile or buildpacks, with branch and pull request build rules, build
  arguments and an optional build plan. Its `id` is what
  `deployment.build_service.id` on jobs and deployment services takes.
- `northflank_combined_service` resource: builds one branch of a Git
  repository and runs it, with the same build settings as a build service and
  the same ports, health checks and runtime environment as a deployment
  service.
- `autoscaling` on `northflank_deployment_service` and
  `northflank_combined_service`: horizontal autoscaling between
  `min_replicas` and `max_replicas`, on CPU, memory, requests per second or
  custom Prometheus metrics. At least one trigger is required and the replica
  range is checked at plan time.
- `northflank_volume` resource: a persistent volume with its size, storage
  class, access mode, mounts and the services or jobs it is attached to.
  Size and mounts update in place; attachments are reconciled through the
  attach and detach endpoints. Supports import as `<project_id>/<volume_id>`.
- `northflank_volume` data source: looks up a volume by `id` or `name` within
  a project.
- `northflank_addon` resource: a native Northflank addon (PostgreSQL, MySQL,
  MongoDB, Redis and so on) with its version, plan, replicas, storage and
  networking, and its credentials as sensitive computed maps. Create and
  update wait until the addon runs; destroy waits until it is gone. Updates
  are PATCHes carrying only what changed. Supports import as
  `<project_id>/<addon_id>`.
- `northflank_addon` data source: looks up an addon by `id` or `name` within
  a project, including its credentials.
- `northflank_domain` and `northflank_subdomain` resources: register a root
  domain and add subdomains to it, exposing the TXT and CNAME records to
  create. Import as `<domain>` and `<domain>/<subdomain>`.
- `northflank_domain_verification` and `northflank_subdomain_verification`
  resources: retry verification until the DNS record has propagated or a
  configurable timeout passes, so that a DNS provider can create the record
  in the same apply.
- `northflank_domain` data source: a registered domain's status,
  verification record and subdomains.
- `northflank_preview_blueprint` resource: a preview blueprint from a JSON
  template, typically read from the repository with `file()`, with argument
  overrides for secrets such as webhook tokens. Terraform delivers the
  template; the blueprint is never linked to the repository through GitOps,
  so Northflank never writes to it. The name comes from the template;
  renaming replaces the blueprint. Supports import as
  `<project_id>/<preview_blueprint_id>`.
- `northflank_preview_blueprint` data source: looks up a blueprint by `id` or
  `name` within a project.
- `northflank_service` data source: looks up a service of any kind by `id` or
  `name` within a project, including its repository and branch.
- Provider scaffold: `token` and `endpoint` configuration, also settable with
  `NORTHFLANK_API_TOKEN` and `NORTHFLANK_ENDPOINT`.
- API client generated with oapi-codegen from Northflank's OpenAPI document,
  vendored from `https://api.northflank.com/v1/swagger-json` and repaired by
  `internal/tools/normalizespec` first.
- The deployment block (external image or build service, plus command and
  entrypoint overrides) is shared by jobs and deployment services, and the
  repository and build blocks by build and combined services.
- Unit tests, an offline smoke plan, CI, and a GoReleaser setup for the
  Terraform Registry.
