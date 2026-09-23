resource "northflank_domain" "root" {
  domain = "example.com"
}

# Create the TXT record northflank_domain.root asks for, with any DNS provider,
# then wait for Northflank to see it.
resource "northflank_domain_verification" "root" {
  domain  = northflank_domain.root.domain
  timeout = "30m"
}
