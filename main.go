// terraform-provider-northflank is a Terraform provider for the Northflank
// platform.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/srs/terraform-provider-northflank/internal/provider"
)

// version is overridden at release time via -ldflags.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/srs/northflank",
		Debug:   debug,
	})

	if err != nil {
		log.Fatal(err.Error())
	}
}
