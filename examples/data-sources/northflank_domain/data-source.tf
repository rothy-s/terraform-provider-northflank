data "northflank_domain" "root" {
  domain = "example.com"
}

output "subdomains" {
  value = [for s in data.northflank_domain.root.subdomains : s.full_name]
}
