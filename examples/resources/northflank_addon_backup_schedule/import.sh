# Backup schedules are imported as <project_id>/<addon_id>/<schedule_id>. The
# schedule IDs of an addon are listed by
# GET /v1/projects/{projectId}/addons/{addonId}/backup-schedules.
terraform import northflank_addon_backup_schedule.db_daily my-application/main-db/62cc20b90956ab62a58e8474
