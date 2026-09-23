# Create the CNAME northflank_subdomain.api asks for, then wait for Northflank
# to see it. full_name is then ready for a service's ports[].domains.
resource "northflank_subdomain_verification" "api" {
  domain    = northflank_subdomain.api.domain
  subdomain = northflank_subdomain.api.name
}
