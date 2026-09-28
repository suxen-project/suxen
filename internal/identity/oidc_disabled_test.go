package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOIDCCallbackRejectsStateWhenBrowserLoginDisabled(t *testing.T) {
	// An empty HMAC key is publicly known. A callback must not accept a
	// transaction signed with it when the operator has disabled browser login.
	transaction := LoginTransaction{
		Provider: "provider", State: "state", Silent: true,
		RedirectPath: "/signed-in", ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}
	payload, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, nil)
	_, _ = mac.Write([]byte(encoded))
	cookie := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	service := New(Options{})
	request := httptest.NewRequest(http.MethodGet,
		"/auth/oidc/provider/callback?state=state&error=login_required", nil)
	request.AddCookie(&http.Cookie{Name: LoginCookieName, Value: cookie})
	response := httptest.NewRecorder()
	service.HandleOIDCLogin(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled callback status = %d; want 503; body = %s", response.Code, response.Body.String())
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Errorf("disabled callback redirected to %q", location)
	}
	if _, err := service.VerifyOIDCLoginTransaction(cookie); err == nil {
		t.Error("verified login state with no configured signing secret")
	}
}
