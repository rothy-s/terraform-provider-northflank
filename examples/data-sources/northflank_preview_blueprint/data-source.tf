data "northflank_preview_blueprint" "pr" {
  project_id = "my-application"
  name       = "PR Previews"
}

output "preview_blueprint_id" {
  value = data.northflank_preview_blueprint.pr.id
}
