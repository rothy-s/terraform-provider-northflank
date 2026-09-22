resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}

# Builds main and release branches, plus pull requests from feature branches.
# The repository must be reachable through a Git account linked to the team.
resource "northflank_build_service" "api" {
  project_id = northflank_project.app.id
  name       = "API Builder"

  repository = {
    url      = "https://github.com/acme/api"
    provider = "github"
  }

  build = {
    dockerfile = {
      path      = "/Dockerfile"
      work_dir  = "/"
      use_cache = true
    }
  }

  branch_restrictions = ["main", "release/*"]
  pr_restrictions     = ["feature/*"]

  build_arguments = {
    NODE_ENV = "production"
  }
}

# Run the latest main build of the builder above.
resource "northflank_deployment_service" "api" {
  project_id      = northflank_project.app.id
  name            = "API"
  deployment_plan = "nf-compute-20"

  deployment = {
    build_service = {
      id     = northflank_build_service.api.id
      branch = "main"
    }
  }

  ports = [
    { name = "http", internal_port = 8080, protocol = "HTTP", public = true },
  ]
}
