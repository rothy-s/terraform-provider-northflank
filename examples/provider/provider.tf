terraform {
  required_providers {
    northflank = {
      source  = "srs/northflank"
      version = "0.1.0"
    }
  }
}

# The token may also be supplied with the NORTHFLANK_API_TOKEN environment
# variable, and the endpoint with NORTHFLANK_ENDPOINT.
provider "northflank" {
  token = var.northflank_token
}

variable "northflank_token" {
  type        = string
  sensitive   = true
  description = "Northflank API token, created in the dashboard under team settings, API, Tokens."
}
