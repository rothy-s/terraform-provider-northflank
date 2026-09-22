data "northflank_volume" "db_data" {
  project_id = "my-application"
  name       = "Postgres Data"
}

output "db_volume_status" {
  value = data.northflank_volume.db_data.status
}
