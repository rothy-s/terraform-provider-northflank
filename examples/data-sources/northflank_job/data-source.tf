# By ID.
data "northflank_job" "by_id" {
  project_id = "my-application"
  id         = "nightly-report"
}

# By name.
data "northflank_job" "by_name" {
  project_id = "my-application"
  name       = "Nightly Report"
}

output "nightly_schedule" {
  value = data.northflank_job.by_name.cron.schedule
}
