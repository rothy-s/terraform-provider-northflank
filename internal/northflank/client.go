package northflank

import (
	"context"
	"net/http"

	"github.com/srs/terraform-provider-northflank/internal/apierr"
)

// DefaultBaseURL is the public Northflank API endpoint. Generated request
// paths already begin with /v1, so the version is not part of it.
const DefaultBaseURL = "https://api.northflank.com"

// APIError is a non-2xx response from the Northflank API. Aliased from the
// shared package so that callers can keep using northflank.APIError.
type APIError = apierr.APIError

// CheckResponse converts a non-2xx response into an *APIError, and returns nil
// for anything in the 2xx range.
func CheckResponse(resp apierr.Response) error {
	return apierr.Check(resp)
}

// IsTransient reports whether an error is worth retrying.
func IsTransient(err error) bool {
	return apierr.IsTransient(err)
}

// NewAuthenticated returns a response-parsing client that attaches the given
// API token to every request as a bearer token.
func NewAuthenticated(baseURL, token string, opts ...ClientOption) (*ClientWithResponses, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	// Put the auth editor first so callers can still override the header.
	opts = append([]ClientOption{WithRequestEditorFn(bearerAuth(token))}, opts...)
	return NewClientWithResponses(baseURL, opts...)
}

func bearerAuth(token string) RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}
