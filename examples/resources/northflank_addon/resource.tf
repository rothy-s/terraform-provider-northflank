resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}

# A two-replica PostgreSQL 16 database. Creating it waits until it is running.
resource "northflank_addon" "db" {
  project_id      = northflank_project.app.id
  name            = "Main DB"
  type            = "postgresql"
  version         = "16"
  deployment_plan = "nf-compute-20"
  replicas        = 2
  storage_size    = 8192
  tls_enabled     = true
  database_name   = "app"

  lifecycle {
    prevent_destroy = true
  }
}

# Hand the connection string to a service. Which keys connection_details has
# depends on the addon type, and the one below is illustrative: after the first
# apply, `terraform console` and `nonsensitive(keys(northflank_addon.db.connection_details))`
# show the real ones. Alternatively, link the addon to a secret group in the
# Northflank dashboard.
resource "northflank_deployment_service" "api" {
  project_id      = northflank_project.app.id
  name            = "API"
  deployment_plan = "nf-compute-20"

  deployment = {
    external_image = {
      image_path = "ghcr.io/acme/api:1.4"
    }
  }

  runtime_environment = {
    DATABASE_URL = northflank_addon.db.connection_details["POSTGRES_URI"]
  }
}
