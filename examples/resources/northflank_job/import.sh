# Jobs are imported as <project_id>/<job_id>. The API does not report the
# deployment source or runtime environment, so the first apply after an import
# sets them from configuration.
terraform import northflank_job.nightly_report my-application/nightly-report
