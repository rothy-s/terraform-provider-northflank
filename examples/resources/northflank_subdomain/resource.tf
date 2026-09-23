resource "northflank_subdomain" "api" {
  domain = northflank_domain_verification.root.domain
  name   = "api"
}

output "api_dns_record" {
  value = "${northflank_subdomain.api.full_name} ${northflank_subdomain.api.record_type} ${northflank_subdomain.api.record_value}"
}
