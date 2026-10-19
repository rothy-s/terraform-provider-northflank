resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}

# Available to every service and job in the project.
resource "northflank_secret_group" "shared" {
  project_id  = northflank_project.app.id
  name        = "Shared Config"
  description = "Settings every service reads."
  type        = "config"

  variables = {
    LOG_LEVEL = "info"
    NODE_ENV  = "production"
  }
}

# Runtime-only secrets, restricted to one service, and merged over the shared
# group thanks to the higher priority.
resource "northflank_secret_group" "api" {
  project_id  = northflank_project.app.id
  name        = "API Secrets"
  priority    = 20
  secret_type = "environment"

  variables = {
    DATABASE_URL = var.database_url
  }

  restrictions = {
    restricted = true
    resources = [
      { id = "api", type = "service" },
    ]
  }
}

# Read in the Dockerfile with RUN --mount=type=secret,id=npmrc.
resource "northflank_secret_group" "build" {
  project_id  = northflank_project.app.id
  name        = "Build Secrets"
  secret_type = "arguments"

  docker_secret_mounts = {
    npmrc = var.npmrc
  }

  restrictions = {
    restricted = true
    resources = [
      { id = "api-builder", type = "service" },
    ]
  }
}

variable "npmrc" {
  type      = string
  sensitive = true
}

variable "database_url" {
  type      = string
  sensitive = true
}
