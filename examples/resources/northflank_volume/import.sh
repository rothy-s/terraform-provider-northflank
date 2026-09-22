# Volumes are imported as <project_id>/<volume_id>. The API does not report
# mounts, so the first apply after an import sets them from configuration.
terraform import northflank_volume.db_data my-application/postgres-data
