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

resource "northflank_job" "cron" {
  project_id      = northflank_project.smoke.id
  name            = "Smoke Cron"
  deployment_plan = "nf-compute-20"

  deployment = {
    external_image = {
      image_path = "busybox:latest"
    }
    command = "echo smoke"
  }

  cron = {
    schedule = "@daily"
  }

  runtime_environment = {
    SMOKE = "true"
  }
}

resource "northflank_job" "manual" {
  project_id      = northflank_project.smoke.id
  name            = "Smoke Manual"
  deployment_plan = "nf-compute-10"

  deployment = {
    build_service = {
      id = "smoke-builder"
    }
    entrypoint = "/bin/sh"
    command    = "-c true"
  }
}

data "northflank_job" "by_id" {
  project_id = northflank_project.smoke.id
  id         = northflank_job.cron.id
}

resource "northflank_deployment_service" "web" {
  project_id      = northflank_project.smoke.id
  name            = "Smoke Web"
  deployment_plan = "nf-compute-20"
  instances       = 2

  autoscaling = {
    min_replicas                = 2
    max_replicas                = 4
    memory_threshold_percentage = 80
    custom_metrics = {
      path    = "/metrics"
      port    = 9090
      metrics = [{ name = "inflight", type = "counter", threshold = 10.5 }]
    }
  }

  deployment = {
    external_image = {
      image_path = "nginx:latest"
    }
  }

  ports = [
    {
      name          = "http"
      internal_port = 80
      protocol      = "HTTP"
      public        = true
    },
  ]

  health_checks = [
    {
      type                  = "livenessProbe"
      protocol              = "TCP"
      port                  = 80
      initial_delay_seconds = 5
      period_seconds        = 10
      timeout_seconds       = 2
      failure_threshold     = 3
    },
  ]

  runtime_environment = {
    SMOKE = "true"
  }
}

resource "northflank_deployment_service" "worker" {
  project_id      = northflank_project.smoke.id
  name            = "Smoke Worker"
  deployment_plan = "nf-compute-10"

  deployment = {
    build_service = {
      id = "smoke-builder"
    }
    command = "node worker.js"
  }
}

data "northflank_service" "by_id" {
  project_id = northflank_project.smoke.id
  id         = northflank_deployment_service.web.id
}

resource "northflank_build_service" "builder" {
  project_id = northflank_project.smoke.id
  name       = "Smoke Builder"

  repository = {
    url      = "https://github.com/acme/smoke"
    provider = "github"
  }

  build = {
    dockerfile = {
      target = "runtime"
    }
  }

  branch_restrictions = ["main"]

  build_arguments = {
    SMOKE = "true"
  }
}

resource "northflank_combined_service" "web" {
  project_id      = northflank_project.smoke.id
  name            = "Smoke Combined"
  deployment_plan = "nf-compute-20"

  repository = {
    url      = "https://github.com/acme/smoke"
    provider = "github"
  }
  branch = "main"

  build = {
    buildpack = {}
  }

  ports = [
    { name = "http", internal_port = 3000, protocol = "HTTP", public = true },
  ]

  runtime_environment = {
    SMOKE = "true"
  }
}

resource "northflank_volume" "data" {
  project_id   = northflank_project.smoke.id
  name         = "Smoke Data"
  storage_size = 4096

  mounts = [
    { container_path = "/data" },
  ]

  attachments = [
    { id = northflank_deployment_service.web.id, type = "service" },
  ]
}

data "northflank_volume" "by_id" {
  project_id = northflank_project.smoke.id
  id         = northflank_volume.data.id
}
