# The full flow, with Cloudflare holding the DNS zone: register the domain,
# publish its TXT record, verify it, add a subdomain, publish its CNAME,
# verify that, and route it to a service port.
resource "northflank_domain" "root" {
  domain = "example.com"
}

resource "cloudflare_dns_record" "verify_root" {
  zone_id = var.cloudflare_zone_id
  name    = northflank_domain.root.verification_record_name
  type    = "TXT"
  content = northflank_domain.root.verification_record_value
  ttl     = 300
}

resource "northflank_domain_verification" "root" {
  domain     = northflank_domain.root.domain
  depends_on = [cloudflare_dns_record.verify_root]
}

resource "northflank_subdomain" "api" {
  # Reading the domain from the verification makes this wait for it.
  domain = northflank_domain_verification.root.domain
  name   = "api"
}

resource "cloudflare_dns_record" "api" {
  zone_id = var.cloudflare_zone_id
  name    = northflank_subdomain.api.full_name
  type    = northflank_subdomain.api.record_type
  content = northflank_subdomain.api.record_value
  ttl     = 300
}

resource "northflank_subdomain_verification" "api" {
  domain     = northflank_subdomain.api.domain
  subdomain  = northflank_subdomain.api.name
  depends_on = [cloudflare_dns_record.api]
}

resource "northflank_deployment_service" "api" {
  project_id      = "my-application"
  name            = "API"
  deployment_plan = "nf-compute-20"

  deployment = {
    external_image = {
      image_path = "ghcr.io/acme/api:1.4"
    }
  }

  ports = [
    {
      name          = "http"
      internal_port = 8080
      protocol      = "HTTP"
      public        = true
      domains       = [northflank_subdomain_verification.api.full_name]
    },
  ]
}

variable "cloudflare_zone_id" {
  type = string
}
