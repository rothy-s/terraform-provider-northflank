# A project in a Northflank-managed region. Northflank derives the project ID
# from the name, here "my-application".
resource "northflank_project" "app" {
  name        = "My Application"
  description = "Services for my application."
  color       = "#EF233C"
  region      = "europe-west"
}

# A project on a bring-your-own-cloud cluster already registered with
# Northflank.
resource "northflank_project" "byoc" {
  name       = "BYOC Project"
  cluster_id = "gcp-cluster-1"

  # Also delete the project's services, jobs and addons on destroy. Without
  # this, destroying a project that is not empty fails.
  delete_child_objects = true
}

output "app_project_id" {
  value = northflank_project.app.id
}
