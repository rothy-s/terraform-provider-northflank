resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}

resource "northflank_deployment_service" "db" {
  project_id      = northflank_project.app.id
  name            = "Postgres"
  deployment_plan = "nf-compute-20"

  deployment = {
    external_image = {
      image_path = "postgres:16"
    }
  }
}

# A 10 GB volume holding the database files. Destroying this resource deletes
# the data, so guard it.
resource "northflank_volume" "db_data" {
  project_id   = northflank_project.app.id
  name         = "Postgres Data"
  storage_size = 10240

  mounts = [
    { container_path = "/var/lib/postgresql/data", volume_path = "pgdata" },
  ]

  attachments = [
    { id = northflank_deployment_service.db.id, type = "service" },
  ]

  lifecycle {
    prevent_destroy = true
  }
}
