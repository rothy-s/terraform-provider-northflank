# Preview blueprints are imported as <project_id>/<preview_blueprint_id>. The
# next apply sends the template and argument overrides from configuration.
terraform import northflank_preview_blueprint.pr my-application/pr-previews
