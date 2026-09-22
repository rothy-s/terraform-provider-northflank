# Offline smoke test configuration, driven by scripts/smoke.sh.
#
# DO NOT APPLY THIS. It is planned, never applied: any values below are
# placeholders, and applying it against a real token would create real,
# billable Northflank resources.
#
# scripts/smoke.sh plans a copy of this directory, so any state left here by a
# stray apply is ignored rather than silently refreshed.
#
# It exercises every resource and data source the provider registers.
# That keeps it hermetic: no credentials and no network access are required,
# because every resource is new and every data source depends on one of them,
# so Terraform defers the reads to apply.
#
# Keep this in step with provider.Resources and provider.DataSources: the
# script asserts that the plan covers every registered type.

terraform {
  required_version = ">= 1.8"

  required_providers {
    northflank = {
      # No version constraint: the smoke test loads the provider through
      # dev_overrides, which ignores them.
      source = "srs/northflank"
    }
  }
}

provider "northflank" {
  # Supplied by the script as NORTHFLANK_API_TOKEN. Never used, since nothing
  # here reaches the API during a plan.
}

resource "northflank_project" "smoke" {
  name        = "Smoke Test"
  description = "Planned by the offline smoke test, never applied."
  color       = "#EF233C"
  region      = "europe-west"
}

resource "northflank_project" "byoc" {
  name                 = "Smoke Test BYOC"
  cluster_id           = "gcp-cluster-1"
  delete_child_objects = true
}

# Keys off an attribute that is unknown until apply, which is what keeps the
# plan offline.
data "northflank_project" "by_id" {
  id = northflank_project.smoke.id
}

resource "northflank_secret_group" "shared" {
  project_id = northflank_project.smoke.id
  name       = "Smoke Shared"
  type       = "config"
  tags       = ["smoke"]

  variables = {
    LOG_LEVEL = "debug"
  }
}

resource "northflank_secret_group" "restricted" {
  project_id  = northflank_project.smoke.id
  name        = "Smoke Restricted"
  priority    = 20
  secret_type = "environment"

  variables = {
    DATABASE_URL = "postgres://placeholder"
  }

  restrictions = {
    restricted          = true
    tags                = ["backend"]
    tag_match_condition = "or"
    resources = [
      { id = "api", type = "service" },
    ]
  }
}

data "northflank_secret_group" "by_id" {
  project_id = northflank_project.smoke.id
  id         = northflank_secret_group.shared.id
}
