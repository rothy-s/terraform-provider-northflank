# By ID.
data "northflank_project" "by_id" {
  id = "my-application"
}

# By name. Names are matched exactly; if several projects share a name, look
# the project up by ID instead.
data "northflank_project" "by_name" {
  name = "My Application"
}

output "project_region" {
  value = data.northflank_project.by_name.region
}
