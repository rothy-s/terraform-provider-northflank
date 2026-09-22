resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}

# Builds main with buildpacks and runs it, redeploying on every push.
resource "northflank_combined_service" "web" {
  project_id      = northflank_project.app.id
  name            = "Web"
  deployment_plan = "nf-compute-20"
  instances       = 2

  repository = {
    url      = "https://github.com/acme/web"
    provider = "github"
  }
  branch = "main"

  build = {
    buildpack = {
      build_context = "/web"
    }
  }

  command = "npm start"

  ports = [
    { name = "http", internal_port = 3000, protocol = "HTTP", public = true },
  ]

  health_checks = [
    {
      type                  = "readinessProbe"
      protocol              = "HTTP"
      path                  = "/"
      port                  = 3000
      initial_delay_seconds = 10
      period_seconds        = 10
      timeout_seconds       = 2
      failure_threshold     = 3
    },
  ]

  runtime_environment = {
    PORT = "3000"
  }
}
