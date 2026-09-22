# Any kind of service: deployment, combined or build.
data "northflank_service" "api" {
  project_id = "my-application"
  name       = "API"
}

# Run a job from the same image as an existing service.
resource "northflank_job" "backfill" {
  project_id      = "my-application"
  name            = "Backfill"
  deployment_plan = data.northflank_service.api.deployment_plan

  deployment = {
    external_image = {
      image_path = data.northflank_service.api.image_path
    }
    command = "node backfill.js"
  }
}
