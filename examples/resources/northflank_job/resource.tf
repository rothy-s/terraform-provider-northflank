resource "northflank_project" "app" {
  name   = "My Application"
  region = "europe-west"
}

# A cron job running a public image every night at 02:30.
resource "northflank_job" "nightly_report" {
  project_id      = northflank_project.app.id
  name            = "Nightly Report"
  deployment_plan = "nf-compute-20"

  deployment = {
    external_image = {
      image_path = "ghcr.io/acme/report:1.2"
    }
    command = "node report.js"
  }

  cron = {
    schedule           = "30 2 * * *"
    concurrency_policy = "forbid"
  }

  backoff_limit           = 2
  active_deadline_seconds = 3600

  runtime_environment = {
    REPORT_BUCKET = "s3://acme-reports"
  }
}

# A manual job running the latest build of a Northflank build service. Trigger
# it from the dashboard, the CLI or the API.
resource "northflank_job" "migrate" {
  project_id      = northflank_project.app.id
  name            = "Database Migration"
  deployment_plan = "nf-compute-10"

  deployment = {
    build_service = {
      id     = "api-builder"
      branch = "main"
    }
    command = "npm run migrate"
  }
}
