data "northflank_addon" "db" {
  project_id = "my-application"
  name       = "Main DB"
}

output "db_password" {
  value     = data.northflank_addon.db.secrets["password"]
  sensitive = true
}
