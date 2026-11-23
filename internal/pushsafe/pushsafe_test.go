package pushsafe

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidateEndpoint(t *testing.T) {
	valid := []string{
		"https://fcm.googleapis.com/fcm/send/abc123",
		"https://web.push.apple.com/QGxhbGxvd2Vk",
		"https://updates.push.services.mozilla.com/wpush/v2/abc123",
		"https://push.services.mozilla.com/wpush/v1/abc123",
		"https://wns2-par02p.notify.windows.com/w/?token=abc123",
	}
	for _, raw := range valid {
		if err := ValidateEndpoint(raw); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v, want nil", raw, err)
		}
	}

	invalid := []string{
		"",
		"http://fcm.googleapis.com/fcm/send/abc",
		"https://fcm.googleapis.com",
		"https://fcm.googleapis.com/",
		"https://user:pass@fcm.googleapis.com/fcm/send/abc",
		"https://fcm.googleapis.com:8443/fcm/send/abc",
		"https://example.com/push/abc",
		"https://169.254.169.254/latest/meta-data",
		"https://127.0.0.1/push",
		"https://fcm.googleapis.com.evil.test/fcm/send/abc",
		"https://evilfcm.googleapis.com/fcm/send/abc",
		"https://notify.windows.com.evil.test/push",
		"https://evilnotify.windows.com/push",
		"https://" + strings.Repeat("a", 2049),
	}
	for _, raw := range invalid {
		if err := ValidateEndpoint(raw); err == nil {
			t.Errorf("ValidateEndpoint(%q) = nil, want rejection", raw)
		}
	}
}

func TestAllowedHostBoundary(t *testing.T) {
	if AllowedHost("evil-fcm.googleapis.com") {
		t.Error("suffix matching must not accept an attacker-owned prefix")
	}
	if AllowedHost("notify.windows.com.evil.test") {
		t.Error("suffix matching must be anchored at the end")
	}
	if !AllowedHost("WNS2-PAR02P.NOTIFY.WINDOWS.COM.") {
		t.Error("allowed host should be case-insensitive and tolerate a trailing dot")
	}
}

func TestHostOnlyForLogs(t *testing.T) {
	if got := Host("https://fcm.googleapis.com/fcm/send/secret-token"); got != "fcm.googleapis.com" {
		t.Errorf("Host = %q, want fcm.googleapis.com", got)
	}
	if got := Host("://bad"); got != "" {
		t.Errorf("Host(invalid) = %q, want empty", got)
	}
}

func TestHTTPClientRejectsOffAllowlistRedirect(t *testing.T) {
	c := NewHTTPClient(time.Second)
	if c.CheckRedirect == nil {
		t.Fatal("push client has no redirect policy")
	}
	req, err := http.NewRequest(http.MethodGet, "https://169.254.169.254/latest/meta-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Fatal("redirect to a metadata address was allowed")
	}
	ok, err := http.NewRequest(http.MethodGet, "https://fcm.googleapis.com/fcm/send/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(ok, nil); err != nil {
		t.Fatalf("redirect to an allowed push host rejected: %v", err)
	}
}
