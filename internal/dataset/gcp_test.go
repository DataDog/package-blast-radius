package dataset

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The whole point of the hint is that it can be copied and run.
func TestAuthHintNamesTheCommandToRun(t *testing.T) {
	if !strings.Contains(authHint, "gcloud auth login --update-adc") {
		t.Errorf("authHint does not name the login command:\n%s", authHint)
	}
	if !strings.Contains(authHint, "GOOGLE_APPLICATION_CREDENTIALS") {
		t.Errorf("authHint does not mention the service account path:\n%s", authHint)
	}
}

func TestAPIErrorAddsTheAuthHintWhenCredentialsAreRejected(t *testing.T) {
	rejected := []*apiError{
		{Status: http.StatusUnauthorized, Message: "Invalid Credentials", Reason: "authError"},
		{Status: http.StatusUnauthorized, Message: "Login Required"},
		{Status: http.StatusForbidden, Message: "token expired", Reason: "accessTokenExpired"},
	}
	for _, e := range rejected {
		if !strings.Contains(e.Error(), "gcloud auth login --update-adc") {
			t.Errorf("HTTP %d reason %q should suggest re-authenticating:\n%s", e.Status, e.Reason, e.Error())
		}
	}
}

// A plain 403 is usually a missing IAM permission on a valid identity, and
// telling the user to log in again would send them the wrong way.
func TestAPIErrorLeavesOtherFailuresAlone(t *testing.T) {
	unrelated := []*apiError{
		{Status: http.StatusForbidden, Message: "insufficient permission to create buckets", Reason: "forbidden"},
		{Status: http.StatusForbidden, Message: "BigQuery API has not been used in this project", Reason: "accessNotConfigured"},
		{Status: http.StatusNotFound, Message: "Not Found", Reason: "notFound"},
		{Status: http.StatusTooManyRequests, Message: "quota exceeded", Reason: "rateLimitExceeded"},
	}
	for _, e := range unrelated {
		if strings.Contains(e.Error(), "gcloud auth") {
			t.Errorf("HTTP %d reason %q should not blame credentials:\n%s", e.Status, e.Reason, e.Error())
		}
		// The API's own explanation still has to survive.
		if !strings.Contains(e.Error(), e.Message) {
			t.Errorf("error %q lost Google's message", e.Error())
		}
	}
}

// A 401 mid-run has to reach the user with the hint attached, not just the code.
func TestUnauthorizedRequestSurfacesTheAuthHint(t *testing.T) {
	fake := newFakeGCP(t)
	fake.dryRunStatus = http.StatusUnauthorized
	c := clientForFake(fake)

	_, err := c.estimateBytes(context.Background(), "proj", "SELECT 1")
	if err == nil {
		t.Fatal("estimateBytes succeeded on a 401")
	}
	if !strings.Contains(err.Error(), "gcloud auth login --update-adc") {
		t.Errorf("a 401 should tell the user how to authenticate:\n%s", err)
	}
}

// Without credentials there is nothing to prompt about, so the failure has to
// arrive before the run starts printing progress.
func TestDownloadFailsWithAuthGuidanceWhenCredentialsAreMissing(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/key.json")
	// CLOUDSDK_CONFIG points the ADC lookup at an empty directory, so a developer
	// machine with a real gcloud login still exercises the missing-credentials path.
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())

	if _, err := newAuthedClient(context.Background()); err == nil {
		t.Skip("credentials were found anyway; this environment cannot test the missing case")
	} else if !strings.Contains(err.Error(), "gcloud auth login --update-adc") {
		t.Errorf("missing credentials should print the login command:\n%s", err)
	}
}
