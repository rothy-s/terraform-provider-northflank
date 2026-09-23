resource "northflank_addon" "db" {
  project_id      = "my-application"
  name            = "main-db"
  type            = "postgresql"
  version         = "18"
  deployment_plan = "nf-compute-20"
  storage_size    = 8192
}

# A dump of the database every day at 03:00, kept for 14 days.
resource "northflank_addon_backup_schedule" "db_daily" {
  project_id     = northflank_addon.db.project_id
  addon_id       = northflank_addon.db.id
  backup_type    = "dump"
  interval       = "daily"
  hours          = [3]
  minutes        = [0]
  retention_days = 14
}

# And a weekly one on Sundays, kept for 12 weeks.
resource "northflank_addon_backup_schedule" "db_weekly" {
  project_id       = northflank_addon.db.project_id
  addon_id         = northflank_addon.db.id
  backup_type      = "dump"
  interval         = "weekly"
  days             = [6]
  hours            = [4]
  minutes          = [0]
  retention_days   = 84
  compression_type = "zstd"
}
