// Package dataset downloads a package ecosystem's dependency graph from the
// deps.dev BigQuery public dataset and builds a queryable DuckDB database from it.
//
// BigQuery and Cloud Storage are reached over their REST APIs rather than through
// the Cloud SDKs, which would add 55 modules for what amounts to a handful of
// documented endpoints. DuckDB is driven through its CLI, matching how
// internal/blast already queries the resulting database.
package dataset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope covers both BigQuery and Cloud Storage.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

const (
	bigQueryBaseURL = "https://bigquery.googleapis.com/bigquery/v2"
	storageBaseURL  = "https://storage.googleapis.com/storage/v1"
)

// authHint is the fix for every credential problem, printed with the failure that
// prompted it. Both lines are needed: the first is what a person runs, the second
// is what CI uses.
const authHint = `Authenticate to GCP with:

    gcloud auth login --update-adc

That signs you in and writes Application Default Credentials, which is what this
command reads. For a service account instead, point GOOGLE_APPLICATION_CREDENTIALS
at its key file.`

// newAuthedClient builds an HTTP client from Application Default Credentials.
func newAuthedClient(ctx context.Context) (*http.Client, error) {
	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("no Google credentials found.\n\n%s\n\nunderlying error: %w", authHint, err)
	}
	// Minting a token here turns expired or revoked credentials into this message
	// rather than an opaque transport failure on the first API call, which would
	// otherwise arrive after the run had already started reporting progress.
	if _, err := creds.TokenSource.Token(); err != nil {
		return nil, fmt.Errorf("found Google credentials but could not get a token from them; they have most likely expired.\n\n%s\n\nunderlying error: %w",
			authHint, err)
	}
	return oauth2.NewClient(ctx, creds.TokenSource), nil
}

// apiError carries the message Google returned, which is far more useful than
// the bare status code (quota exceeded, API not enabled, permission denied).
type apiError struct {
	Status  int
	Message string
	Reason  string
	URL     string
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Reason != "" {
		msg = fmt.Sprintf("%s (HTTP %d, reason %s)", msg, e.Status, e.Reason)
	} else {
		msg = fmt.Sprintf("%s (HTTP %d)", msg, e.Status)
	}
	if e.needsReauth() {
		msg += "\n\n" + authHint
	}
	return msg
}

// needsReauth reports whether the API rejected who we are rather than what we
// asked for. A plain 403 is usually a missing IAM permission on a perfectly valid
// identity, so telling the user to re-authenticate would send them the wrong way;
// only the statuses and reasons Google uses for credential problems qualify.
func (e *apiError) needsReauth() bool {
	if e.Status == http.StatusUnauthorized {
		return true
	}
	switch e.Reason {
	case "authError", "unauthorized", "invalidCredentials", "accessTokenExpired", "invalid_grant":
		return true
	}
	return false
}

func hasStatus(err error, status int) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// isNotFound distinguishes "the bucket does not exist yet", which is a normal
// branch, from a permission or quota problem, which is not.
func isNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

func isAlreadyExists(err error) bool { return hasStatus(err, http.StatusConflict) }

// googleErrorEnvelope is the shape Google's JSON APIs use for failures.
type googleErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// client wraps the authenticated HTTP client with JSON request/response plumbing.
type client struct {
	http *http.Client
	// bigQueryURL and storageURL are fields rather than constants so tests can
	// point them at an httptest.Server.
	bigQueryURL string
	storageURL  string
}

func newClient(httpClient *http.Client) *client {
	return &client{
		http:        httpClient,
		bigQueryURL: bigQueryBaseURL,
		storageURL:  storageBaseURL,
	}
}

func (c *client) doJSON(ctx context.Context, method, url string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp, url)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", url, err)
	}
	return nil
}

// decodeAPIError reads Google's error envelope, falling back to the raw body
// when the response is not that shape (a proxy or an HTML error page).
func decodeAPIError(resp *http.Response, url string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var envelope googleErrorEnvelope
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		e := &apiError{Status: resp.StatusCode, Message: envelope.Error.Message, URL: url}
		if len(envelope.Error.Errors) > 0 {
			e.Reason = envelope.Error.Errors[0].Reason
		}
		return e
	}

	message := strings.TrimSpace(string(raw))
	if len(message) > 300 {
		message = message[:300] + "..."
	}
	return &apiError{Status: resp.StatusCode, Message: message, URL: url}
}
