# By ID.
data "northflank_secret_group" "by_id" {
  project_id = "my-application"
  id         = "api-secrets"
}

# By name. variables holds the group's own decrypted values.
data "northflank_secret_group" "by_name" {
  project_id = "my-application"
  name       = "API Secrets"
}

output "database_url" {
  value     = data.northflank_secret_group.by_name.variables["DATABASE_URL"]
  sensitive = true
}
