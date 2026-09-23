package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srs/terraform-provider-northflank/internal/northflank"
)

func shortVerification(t *testing.T) {
	t.Helper()

	interval := verificationPollInterval
	verificationPollInterval = time.Millisecond
	t.Cleanup(func() { verificationPollInterval = interval })
}

// fakeDomainAPI verifies example.com on the third attempt, rejects
// forbidden.com outright, and never verifies slow.com.
func fakeDomainAPI(t *testing.T) (*northflank.ClientWithResponses, *atomic.Int32) {
	t.Helper()

	var attempts atomic.Int32

	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/domains/{domain}/verify", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("domain") {
		case "forbidden.com":
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"error":{"message":"missing permission"}}`)
		case "slow.com":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"TXT record not found"}}`)
		default:
			if attempts.Add(1) < 3 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":{"message":"TXT record not found"}}`)
				return
			}

			_, _ = fmt.Fprint(w, `{"data":{}}`)
		}
	})

	mux.HandleFunc("GET /v1/domains/example.com", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"name":"example.com","status":"verified",
			"hostname":"_northflank-verify.example.com","token":"nf-verify=abc123",
			"subdomains":[{"name":"api","fullName":"api.example.com"}]}}`)
	})

	mux.HandleFunc("GET /v1/domains/example.com/subdomains/api", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"name":"api","fullName":"api.example.com","content":"lb.northflank.app",
			"recordType":"CNAME","verified":false,"routingMode":"paths"}}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := northflank.NewAuthenticated(server.URL, "tok")

	if err != nil {
		t.Fatal(err)
	}

	return client, &attempts
}

func verifyDomain(client *northflank.ClientWithResponses, domain string) func(context.Context) error {
	return func(ctx context.Context) error {
		got, err := client.PostDomainsDomainVerifyWithResponse(ctx, domain)

		if err != nil {
			return err
		}

		return northflank.CheckResponse(got)
	}
}

func TestVerifyUntil(t *testing.T) {
	shortVerification(t)
	ctx := t.Context()
	client, attempts := fakeDomainAPI(t)

	t.Run("retries until DNS propagates", func(t *testing.T) {
		if err := verifyUntil(ctx, 5*time.Second, "domain", verifyDomain(client, "example.com")); err != nil {
			t.Fatalf("verifyUntil() = %v", err)
		}

		if attempts.Load() != 3 {
			t.Errorf("attempts = %d, want 3", attempts.Load())
		}
	})

	t.Run("does not retry other refusals", func(t *testing.T) {
		err := verifyUntil(ctx, 5*time.Second, "domain", verifyDomain(client, "forbidden.com"))

		if err == nil || !strings.Contains(err.Error(), "missing permission") {
			t.Errorf("verifyUntil() = %v, want the 403 passed through", err)
		}
	})

	t.Run("gives up with the last reason", func(t *testing.T) {
		err := verifyUntil(ctx, 30*time.Millisecond, "domain \"slow.com\"", verifyDomain(client, "slow.com"))

		if err == nil || !strings.Contains(err.Error(), "within 30ms") || !strings.Contains(err.Error(), "TXT record not found") {
			t.Errorf("verifyUntil() = %v, want a timeout naming the last refusal", err)
		}
	})
}

func TestDomainReads(t *testing.T) {
	ctx := t.Context()
	client, _ := fakeDomainAPI(t)

	domain, err := getDomain(ctx, client, "example.com")

	if err != nil {
		t.Fatal(err)
	}

	var model domainResourceModel
	applyDomain(&model, domain)

	if model.VerificationRecordName.ValueString() != "_northflank-verify.example.com" ||
		model.VerificationRecordValue.ValueString() != "nf-verify=abc123" || model.Status.ValueString() != "verified" {
		t.Errorf("domain model = %+v", model)
	}

	subdomain, err := getSubdomain(ctx, client, "example.com", "api")

	if err != nil {
		t.Fatal(err)
	}

	var sub subdomainResourceModel
	applySubdomain(&sub, "example.com", "api", subdomain)

	if sub.ID.ValueString() != "example.com/api" || sub.RecordValue.ValueString() != "lb.northflank.app" ||
		sub.RecordType.ValueString() != "CNAME" || sub.Verified.ValueBool() || sub.FullName.ValueString() != "api.example.com" {
		t.Errorf("subdomain model = %+v", sub)
	}

	if _, err := getDomain(ctx, client, "missing.com"); !isNotFound(err) {
		t.Errorf("getDomain(missing.com) = %v, want a 404", err)
	}
}

func TestParseSubdomainID(t *testing.T) {
	domain, sub, err := parseSubdomainID("example.com/eu.api")

	if err != nil || domain != "example.com" || sub != "eu.api" {
		t.Errorf("parseSubdomainID() = %q, %q, %v", domain, sub, err)
	}

	for _, bad := range []string{"example.com", "/api", "example.com/", "a/b/c"} {
		if _, _, err := parseSubdomainID(bad); err == nil {
			t.Errorf("parseSubdomainID(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseVerificationTimeout(t *testing.T) {
	if d, err := parseVerificationTimeout(""); err != nil || d != defaultVerificationTimeout {
		t.Errorf("default = %v, %v", d, err)
	}

	if d, err := parseVerificationTimeout("45m"); err != nil || d != 45*time.Minute {
		t.Errorf("45m = %v, %v", d, err)
	}

	for _, bad := range []string{"soon", "-5m", "0s"} {
		if _, err := parseVerificationTimeout(bad); err == nil {
			t.Errorf("parseVerificationTimeout(%q) succeeded", bad)
		}
	}
}

func TestDomainPatterns(t *testing.T) {
	for value, want := range map[string]bool{
		"example.com": true, "my-app.co.uk": true, "www.example.com": true, "localhost": false, "a.b": false,
	} {
		if got := domainNamePattern.MatchString(value); got != want {
			t.Errorf("domain %q: match = %v, want %v", value, got, want)
		}
	}

	for value, want := range map[string]bool{
		"api": true, "eu.api": true, "@": true, "*": true, "API": false, "-api": false, "api-": false,
	} {
		if got := subdomainPattern.MatchString(value); got != want {
			t.Errorf("subdomain %q: match = %v, want %v", value, got, want)
		}
	}
}
