package northflank

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubResponse stands in for a generated *HTTPResponse wrapper.
type stubResponse struct {
	status int
	body   []byte
}

func (s stubResponse) StatusCode() int { return s.status }
func (s stubResponse) GetBody() []byte { return s.body }

func TestCheckResponse(t *testing.T) {
	tests := []struct {
		name    string
		resp    stubResponse
		wantErr bool
		wantMsg string
	}{
		{
			name: "200 is not an error",
			resp: stubResponse{status: http.StatusOK, body: []byte(`{"data":{}}`)},
		},
		{
			name: "204 is not an error",
			resp: stubResponse{status: http.StatusNoContent},
		},
		{
			name:    "structured error body is unpacked",
			resp:    stubResponse{status: http.StatusConflict, body: []byte(`{"error":{"status":409,"message":"project already exists","id":"req-1"}}`)},
			wantErr: true,
			wantMsg: "HTTP 409: project already exists (request req-1)",
		},
		{
			name: "validation details are passed on",
			resp: stubResponse{status: http.StatusBadRequest, body: []byte(`{"error":{"status":400,` +
				`"message":"Request failed payload validation - see details.","id":"nf-invalid-request-body",` +
				`"details":{"healthChecks.0.successThreshold":["must be 1 for liveness probes"]}}}`)},
			wantErr: true,
			wantMsg: "HTTP 400: Request failed payload validation - see details. (request nf-invalid-request-body)" +
				"\nDetails: {\"details\":{\"healthChecks.0.successThreshold\":[\"must be 1 for liveness probes\"]}}",
		},
		{
			name: "top-level details are passed on too",
			resp: stubResponse{status: http.StatusBadRequest, body: []byte(`{"error":{"message":"invalid"},` +
				`"details":[{"path":"ports.0.name","message":"too long"}]}`)},
			wantErr: true,
			wantMsg: "HTTP 400: invalid\nDetails: {\"details\":[{\"path\":\"ports.0.name\",\"message\":\"too long\"}]}",
		},
		{
			name:    "error without a request id",
			resp:    stubResponse{status: http.StatusBadRequest, body: []byte(`{"error":{"status":400,"message":"bad name"}}`)},
			wantErr: true,
			wantMsg: "HTTP 400: bad name",
		},
		{
			name:    "top-level message",
			resp:    stubResponse{status: http.StatusForbidden, body: []byte(`{"message":"nope"}`)},
			wantErr: true,
			wantMsg: "HTTP 403: nope",
		},
		{
			name:    "unparseable body falls back to raw text",
			resp:    stubResponse{status: http.StatusInternalServerError, body: []byte("upstream exploded")},
			wantErr: true,
			wantMsg: "HTTP 500: upstream exploded",
		},
		{
			name:    "empty body",
			resp:    stubResponse{status: http.StatusBadGateway},
			wantErr: true,
			wantMsg: "HTTP 502 (empty response body)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckResponse(tt.resp)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("CheckResponse() = %v, want nil", err)
				}

				return
			}

			if err == nil {
				t.Fatal("CheckResponse() = nil, want error")
			}

			if got := err.Error(); got != tt.wantMsg {
				t.Errorf("error message = %q, want %q", got, tt.wantMsg)
			}

			var apiErr *APIError

			if !errors.As(err, &apiErr) {
				t.Fatalf("error is %T, want *APIError", err)
			}

			if apiErr.StatusCode != tt.resp.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tt.resp.status)
			}
		})
	}
}

func TestAPIErrorIsNotFound(t *testing.T) {
	notFound := CheckResponse(stubResponse{status: http.StatusNotFound, body: []byte(`{"error":{"message":"not found"}}`)})

	var apiErr *APIError

	if !errors.As(notFound, &apiErr) {
		t.Fatalf("error is %T, want *APIError", notFound)
	}

	if !apiErr.IsNotFound() {
		t.Error("IsNotFound() = false for a 404, want true")
	}

	other := CheckResponse(stubResponse{status: http.StatusConflict})

	if errors.As(other, &apiErr) && apiErr.IsNotFound() {
		t.Error("IsNotFound() = true for a 409, want false")
	}
}

// A round trip through the generated client against a fake server: proves the
// token is sent, generated paths carry the /v1 prefix on top of the base URL,
// and the {"data": ...} envelope decodes.
func TestGeneratedClientRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/projects"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}

		if got, want := r.Header.Get("Authorization"), "Bearer nf_123"; got != want {
			t.Errorf("Authorization header = %q, want %q", got, want)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"projects":[{"id":"default-project","uid":"68f0aa1b9c4e2d7f11a3b402","name":"Default Project"}]},"pagination":{"hasNextPage":false,"count":1}}`))
	}))
	defer server.Close()

	client, err := NewAuthenticated(server.URL, "nf_123")

	if err != nil {
		t.Fatalf("NewAuthenticated() error = %v", err)
	}

	resp, err := client.GetProjectsWithResponse(t.Context(), &GetProjectsParams{})

	if err != nil {
		t.Fatalf("GetProjectsWithResponse() error = %v", err)
	}

	if err := CheckResponse(resp); err != nil {
		t.Fatalf("CheckResponse() = %v", err)
	}

	if resp.JSON200 == nil || len(resp.JSON200.Data.Projects) != 1 {
		t.Fatalf("JSON200 = %+v, want one project", resp.JSON200)
	}

	if got := resp.JSON200.Data.Projects[0].Id; got != "default-project" {
		t.Errorf("project id = %q, want %q", got, "default-project")
	}
}

func TestNewAuthenticatedDefaultsBaseURL(t *testing.T) {
	if _, err := NewAuthenticated("https://northflank.internal", "tok"); err != nil {
		t.Errorf("NewAuthenticated(custom) error = %v", err)
	}

	if _, err := NewAuthenticated("", "tok"); err != nil {
		t.Errorf("NewAuthenticated(default) error = %v", err)
	}
}
