package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireDesignerTokenReacquiresExpiredBrokerToken(t *testing.T) {
	useTemporaryWorkingDirectory(t)
	tm := NewTokenManager("tenant", "client", "scope", "refresh", "cache")
	if err := tm.writeBrokerRefreshToken("expired-token"); err != nil {
		t.Fatalf("write expired broker refresh token: %v", err)
	}

	var requests []string
	tm.designerTokenRequest = func(refreshToken string) (string, int, error) {
		requests = append(requests, refreshToken)
		if refreshToken == "expired-token" {
			return "", 0, &designerOAuthError{
				Status:      http.StatusBadRequest,
				Code:        "invalid_grant",
				Description: "AADSTS700084: refresh token expired",
			}
		}
		return "designer-token", 3600, nil
	}

	acquisitions := 0
	tm.brokerTokenAcquisition = func() (string, error) {
		acquisitions++
		return "replacement-token", nil
	}

	token, expiresIn, err := tm.acquireDesignerToken()
	if err != nil {
		t.Fatalf("acquire designer token: %v", err)
	}
	if token != "designer-token" || expiresIn != 3600 {
		t.Fatalf("unexpected token result: token=%q expiresIn=%d", token, expiresIn)
	}
	if acquisitions != 1 {
		t.Fatalf("expected one SSO acquisition, got %d", acquisitions)
	}
	if len(requests) != 2 || requests[0] != "expired-token" || requests[1] != "replacement-token" {
		t.Fatalf("unexpected refresh token requests: %v", requests)
	}
}

func TestAcquireDesignerTokenDoesNotReacquireTransientFailure(t *testing.T) {
	useTemporaryWorkingDirectory(t)
	tm := NewTokenManager("tenant", "client", "scope", "refresh", "cache")
	if err := tm.writeBrokerRefreshToken("existing-token"); err != nil {
		t.Fatalf("write broker refresh token: %v", err)
	}

	transientErr := &designerOAuthError{
		Status:      http.StatusInternalServerError,
		Code:        "temporarily_unavailable",
		Description: "retry later",
	}
	tm.designerTokenRequest = func(string) (string, int, error) {
		return "", 0, transientErr
	}
	tm.brokerTokenAcquisition = func() (string, error) {
		t.Fatal("SSO acquisition must not run for transient failures")
		return "", nil
	}

	_, _, err := tm.acquireDesignerToken()
	if !errors.Is(err, transientErr) {
		t.Fatalf("expected transient error, got %v", err)
	}
}

func TestM365CookieHeaderFiltersCookiesByDomain(t *testing.T) {
	useTemporaryWorkingDirectory(t)
	writeM365Cookies(t, []SSOCookie{
		{Name: "ESTSAUTH", Value: "login-secret", Domain: "login.microsoftonline.com"},
		{Name: "M365Session", Value: "m365-secret", Domain: ".m365.cloud.microsoft"},
		{Name: "Empty", Value: "", Domain: "m365.cloud.microsoft"},
	})

	tm := NewTokenManager("tenant", "client", "scope", "refresh", "cache")
	header, err := tm.M365CookieHeader()
	if err != nil {
		t.Fatalf("build M365 cookie header: %v", err)
	}
	if header != "M365Session=m365-secret" {
		t.Fatalf("unexpected M365 cookie header: %q", header)
	}
}

func TestM365CookieHeaderRequiresM365DomainCookie(t *testing.T) {
	useTemporaryWorkingDirectory(t)
	writeM365Cookies(t, []SSOCookie{{Name: "ESTSAUTH", Value: "secret", Domain: "login.microsoftonline.com"}})

	tm := NewTokenManager("tenant", "client", "scope", "refresh", "cache")
	_, err := tm.M365CookieHeader()
	if !errors.Is(err, ErrM365CookiesUnavailable) {
		t.Fatalf("expected M365 cookies unavailable, got %v", err)
	}
}

func TestSummarizeBrokerAuthorizeResponsePrefersAADSTSError(t *testing.T) {
	body := `<!DOCTYPE html><html><head><title>Something went wrong</title></head><body>
<p>AADSTS50011: The reply URL specified in the request does not match the reply URLs configured for the application.</p>
</body></html>`

	summary := summarizeBrokerAuthorizeResponse(body)
	if !strings.Contains(summary, "AADSTS50011") {
		t.Fatalf("expected AADSTS error in summary, got %q", summary)
	}
	if strings.Contains(summary, "<") {
		t.Fatalf("expected summary without HTML markup, got %q", summary)
	}
}

func TestSummarizeBrokerAuthorizeResponseUsesTitleFallback(t *testing.T) {
	summary := summarizeBrokerAuthorizeResponse("<html><head><title>Something went wrong</title></head><body></body></html>")
	if summary != "page title: Something went wrong" {
		t.Fatalf("unexpected fallback summary: %q", summary)
	}
}

func writeM365Cookies(t *testing.T, cookies []SSOCookie) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"domain":  "m365.cloud.microsoft",
		"cookies": cookies,
	})
	if err != nil {
		t.Fatalf("marshal M365 cookies: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(m365CookiesFile), 0700); err != nil {
		t.Fatalf("create M365 cookie directory: %v", err)
	}
	if err := os.WriteFile(m365CookiesFile, data, 0600); err != nil {
		t.Fatalf("write M365 cookies: %v", err)
	}
}

func useTemporaryWorkingDirectory(t *testing.T) {
	t.Helper()
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	if err := os.MkdirAll(filepath.Dir(designerBrokerRefreshFile), 0700); err != nil {
		t.Fatalf("create token directory: %v", err)
	}
}
