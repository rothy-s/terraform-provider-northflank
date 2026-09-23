package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

// Constraints copied from the OpenAPI document.
var (
	domainNamePattern = regexp.MustCompile(`^((www(\.[a-zA-Z0-9-]{2,})+\.)?[a-zA-Z0-9-]{2,})(\.([a-zA-Z0-9-]{2,}))+$`)
	subdomainPattern  = regexp.MustCompile(`^(\*|@|([0-9a-z]([0-9a-z-]*[0-9a-z])?\.)*[0-9a-z]([0-9a-z-]*[0-9a-z])?)$`)

	subdomainRoutingModes = []string{"paths", "geoRouting", "loadBalancerSubdomain"}
)

// How long verification keeps trying by default, and how often. The poll
// interval is a variable so that tests can shorten it.
var (
	defaultVerificationTimeout = 20 * time.Minute
	verificationPollInterval   = 15 * time.Second
)

// domainRead is the part of the get-domain response the provider uses.
type domainRead struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Hostname   *string           `json:"hostname"`
	Token      *string           `json:"token"`
	Subdomains []domainSubdomain `json:"subdomains"`
}

type domainSubdomain struct {
	Name     string `json:"name"`
	FullName string `json:"fullName"`
}

// subdomainRead is the part of the get-subdomain response the provider uses.
type subdomainRead struct {
	Name        string `json:"name"`
	FullName    string `json:"fullName"`
	Content     string `json:"content"`
	RecordType  string `json:"recordType"`
	Verified    bool   `json:"verified"`
	RoutingMode string `json:"routingMode"`
}

func getDomain(ctx context.Context, client *northflank.ClientWithResponses, domain string) (*domainRead, error) {
	var read *domainRead

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetDomainsDomainWithResponse(ctx, domain)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		read, err = decodeData[domainRead](got.Body, "domain")
		return err
	})

	return read, err
}

func getSubdomain(ctx context.Context, client *northflank.ClientWithResponses, domain, subdomain string) (*subdomainRead, error) {
	var read *subdomainRead

	err := apierr.Retry(ctx, retryAttempts, retryBaseDelay, func() error {
		got, err := client.GetDomainsDomainSubdomainsSubdomainWithResponse(ctx, domain, subdomain)

		if err != nil {
			return err
		}

		if err := northflank.CheckResponse(got); err != nil {
			return err
		}

		read, err = decodeData[subdomainRead](got.Body, "subdomain")
		return err
	})

	return read, err
}

// decodeData unwraps the {"data": ...} envelope of a response body.
func decodeData[T any](body []byte, what string) (*T, error) {
	var envelope struct {
		Data *T `json:"data"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", what, err)
	}

	if envelope.Data == nil {
		return nil, fmt.Errorf("the API returned a successful status with no %s body", what)
	}

	return envelope.Data, nil
}

// verificationRecordName returns the full DNS name of a domain's TXT
// record. Northflank reports it relative to the domain (verify-abc for
// verify-abc.example.com), which a DNS provider would place at the zone apex
// when the domain is itself a subdomain of the zone.
func verificationRecordName(hostname *string, domain string) *string {
	if hostname == nil || *hostname == "" {
		return hostname
	}

	name := strings.TrimSuffix(*hostname, ".")

	if name == domain || strings.HasSuffix(name, "."+domain) {
		return &name
	}

	name += "." + domain
	return &name
}

// notVerifiableYet reports whether a verification failure means the DNS
// record is missing or not yet propagated. Northflank answers that with a
// 400, or with a 404 that is told apart from a missing domain by its message:
// "Could not verify TXT record" for a domain, "Subdomain ... could not be
// verified" for a subdomain. The latter includes seeing a Cloudflare proxy,
// which a proxied wildcard record answers until the new record propagates.
func notVerifiableYet(err error) bool {
	var apiErr *northflank.APIError

	if !errors.As(err, &apiErr) {
		return false
	}

	switch apiErr.StatusCode {
	case http.StatusBadRequest:
		return true
	case http.StatusNotFound:
		message := strings.ToLower(apiErr.Message)
		return strings.Contains(message, "could not verify") || strings.Contains(message, "could not be verified")
	}

	return false
}

// verifyUntil calls verify until it succeeds, the API rejects the request for
// a reason other than "not verifiable yet", or the timeout passes. Transient
// failures are retried by verify itself.
func verifyUntil(ctx context.Context, timeout time.Duration, what string, verify func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last error

	for {
		err := verify(ctx)

		if err == nil {
			return nil
		}

		if ctx.Err() == nil && !notVerifiableYet(err) {
			return err
		}

		if ctx.Err() == nil {
			last = err
		}

		select {
		case <-ctx.Done():
			reason := "the DNS record may be missing, wrong, or not yet propagated"

			if last != nil {
				reason = last.Error()
			}

			return fmt.Errorf("could not verify %s within %s: %s", what, timeout, reason)
		case <-time.After(verificationPollInterval):
		}
	}
}

// parseVerificationTimeout reads the optional timeout attribute.
func parseVerificationTimeout(value string) (time.Duration, error) {
	if value == "" {
		return defaultVerificationTimeout, nil
	}

	d, err := time.ParseDuration(value)

	if err != nil || d <= 0 {
		return 0, fmt.Errorf("timeout %q is not a positive duration such as 20m", value)
	}

	return d, nil
}

// parseSubdomainID splits <domain>/<subdomain>. Neither part contains a
// slash, so the first one separates them.
func parseSubdomainID(id string) (string, string, error) {
	domain, subdomain, ok := strings.Cut(id, "/")

	if !ok || domain == "" || subdomain == "" || strings.Contains(subdomain, "/") {
		return "", "", fmt.Errorf("expected <domain>/<subdomain>, such as example.com/api, got %q", id)
	}

	return domain, subdomain, nil
}
