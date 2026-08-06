package main

import (
	"strings"
	"testing"
)

func TestServeBanner_LANShownWithToken(t *testing.T) {
	lines := serveBanner(4600, true, "192.168.1.42", "s3cr3t-token")
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "http://localhost:4600/?token=s3cr3t-token") {
		t.Errorf("banner missing tokenized localhost URL:\n%s", joined)
	}
	if !strings.Contains(joined, "http://192.168.1.42:4600/?token=s3cr3t-token") {
		t.Errorf("banner missing tokenized LAN URL:\n%s", joined)
	}
	if !strings.Contains(joined, "Token:      s3cr3t-token") {
		t.Errorf("banner missing the printed token:\n%s", joined)
	}
	if !strings.Contains(joined, "NO es gasto real") {
		t.Errorf("banner missing the not-real-spend disclaimer:\n%s", joined)
	}
}

func TestServeBanner_DefaultIsLoopbackOnly(t *testing.T) {
	// No --lan: loopback default, no token, no LAN URL advertised.
	lines := serveBanner(4600, false, "192.168.1.42", "")
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "192.168.1.42") {
		t.Errorf("loopback banner must not advertise a LAN URL:\n%s", joined)
	}
	if strings.Contains(joined, "?token=") {
		t.Errorf("loopback banner must not carry a token:\n%s", joined)
	}
	if !strings.Contains(joined, "solo localhost") {
		t.Errorf("loopback banner should say it's localhost-only:\n%s", joined)
	}
}

func TestServeBanner_LANNoIPv4FallsBack(t *testing.T) {
	lines := serveBanner(4600, true, "", "tok")
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "sin IPv4") {
		t.Errorf("banner should note the missing LAN IPv4:\n%s", joined)
	}
	// The token is still printed so the localhost URL is usable.
	if !strings.Contains(joined, "Token:      tok") {
		t.Errorf("banner should still print the token without an IPv4:\n%s", joined)
	}
}

func TestGenerateToken_UnpredictableURLSafe(t *testing.T) {
	a, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	b, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if a == b {
		t.Fatalf("two tokens collided (%q); crypto/rand should not repeat", a)
	}
	// 128 bits base64url (no padding) => 22 chars.
	if len(a) < 22 {
		t.Errorf("token too short (%d chars, want >= 22): %q", len(a), a)
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("token is not URL-safe: %q", a)
	}
}

func TestLANDashboardURL(t *testing.T) {
	if got := lanDashboardURL("10.0.0.5", 4600, "abc"); got != "http://10.0.0.5:4600/?token=abc" {
		t.Errorf("lanDashboardURL with token = %q", got)
	}
	if got := lanDashboardURL("10.0.0.5", 4600, ""); got != "http://10.0.0.5:4600" {
		t.Errorf("lanDashboardURL without token = %q", got)
	}
}
