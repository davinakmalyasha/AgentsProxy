package api

import "testing"

// TestValidPushKeys_ServiceAllowlist pins the write-time half of the SSRF
// boundary. The send-time half is TestPushSender_RejectsDisallowedEndpoint.
func TestValidPushKeys_ServiceAllowlist(t *testing.T) {
	// Shape-only fixtures: validPushKeys checks lengths here; real P-256
	// validation happens at send. Keep these low-entropy so a secret scanner
	// does not mistake test data for a credential.
	const (
		p256dhFixture = "aaaaaaaaaaaaaaaa"
		authFixture   = "bbbbbbbbbbbbbbbb"
	)
	allowed := []string{
		"https://fcm.googleapis.com/fcm/send/abc123",
		"https://web.push.apple.com/QGxhbGxvd2Vk",
		"https://updates.push.services.mozilla.com/wpush/v2/abc123",
		"https://wns2-par02p.notify.windows.com/w/?token=abc123",
	}
	for _, endpoint := range allowed {
		if !validPushKeys(endpoint, p256dhFixture, authFixture) {
			t.Errorf("validPushKeys(%q) = false, want true", endpoint)
		}
	}

	denied := []string{
		"http://fcm.googleapis.com/fcm/send/abc123",
		"https://example.com/push/abc123",
		"https://169.254.169.254/latest/meta-data",
		"https://127.0.0.1/push/abc123",
		"https://fcm.googleapis.com.evil.test/fcm/send/abc123",
		"https://evilnotify.windows.com/push/abc123",
		"https://fcm.googleapis.com:8443/fcm/send/abc123",
	}
	for _, endpoint := range denied {
		if validPushKeys(endpoint, p256dhFixture, authFixture) {
			t.Errorf("validPushKeys(%q) = true, want false", endpoint)
		}
	}

	if validPushKeys("https://fcm.googleapis.com/fcm/send/abc123", p256dhFixture, "short") {
		t.Error("validPushKeys accepted short auth key material")
	}
}
