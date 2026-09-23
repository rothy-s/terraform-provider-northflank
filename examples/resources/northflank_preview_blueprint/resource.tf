# The blueprint lives in the application repository as JSON. Terraform reads
# it from the checkout it runs in and delivers it to Northflank, so a change
# to the file takes effect on the next apply, for example from CI on merge.
resource "northflank_preview_blueprint" "pr" {
  project_id = "my-application"
  template   = file("${path.module}/../.northflank/preview-blueprint.json")

  # The template references "${args.WEBHOOK_TOKEN}" in its webhook trigger,
  # so the token itself never goes into the repository.
  argument_overrides = {
    WEBHOOK_TOKEN = random_password.preview_webhook.result
  }
}

resource "random_password" "preview_webhook" {
  length  = 40
  special = false
}

# A CI pipeline can also start runs through the API with this ID:
# POST /v1/projects/my-application/preview-blueprints/<id>/runs
output "preview_blueprint_id" {
  value = northflank_preview_blueprint.pr.id
}
