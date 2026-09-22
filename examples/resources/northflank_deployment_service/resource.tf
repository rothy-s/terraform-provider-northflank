resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}

# A public HTTP API running an image from a private registry.
resource "northflank_deployment_service" "api" {
  project_id      = northflank_project.app.id
  name            = "API"
  deployment_plan = "nf-compute-20"
  instances       = 2

  deployment = {
    external_image = {
      image_path     = "ghcr.io/acme/api:1.4"
      credentials_id = "ghcr-credentials"
    }
    command = "node server.js"
  }

  ports = [
    {
      name          = "http"
      internal_port = 8080
      protocol      = "HTTP"
      public        = true
    },
  ]

  health_checks = [
    {
      type                  = "readinessProbe"
      protocol              = "HTTP"
      path                  = "/healthz"
      port                  = 8080
      initial_delay_seconds = 5
      period_seconds        = 10
      timeout_seconds       = 2
      failure_threshold     = 3
    },
  ]

  runtime_environment = {
    LOG_LEVEL = "info"
  }
}

# A private worker running the latest build of a Northflank build service.
resource "northflank_deployment_service" "worker" {
  project_id      = northflank_project.app.id
  name            = "Worker"
  deployment_plan = "nf-compute-10"

  deployment = {
    build_service = {
      id     = "api-builder"
      branch = "main"
    }
    command = "node worker.js"
  }
}
