# Addons are imported as <project_id>/<addon_id>. database_name is not
# reported by the API, so it is taken from configuration afterwards.
terraform import northflank_addon.db my-application/main-db
