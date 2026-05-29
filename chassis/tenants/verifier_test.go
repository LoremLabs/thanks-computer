package tenants

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsPublicGlobalUnicast — table-driven over every reserved range
// we block. New entries land here as the blocklist grows.
func TestIsPublicGlobalUnicast(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		pub  bool
	}{
		{"public v4", "8.8.8.8", true},
		{"public v4 boundary", "9.9.9.9", true},
		{"loopback v4", "127.0.0.1", false},
		{"loopback v4 boundary", "127.255.255.254", false},
		{"loopback v6", "::1", false},
		{"unspecified v4", "0.0.0.0", false},
		{"unspecified v6", "::", false},
		{"10/8", "10.0.0.1", false},
		{"10/8 boundary", "10.255.255.254", false},
		{"172.16/12 low", "172.16.0.1", false},
		{"172.16/12 high", "172.31.255.254", false},
		{"172.32 (not private)", "172.32.0.1", true},
		{"192.168/16", "192.168.1.1", false},
		{"CGNAT 100.64/10 low", "100.64.0.1", false},
		{"CGNAT 100.64/10 high", "100.127.255.254", false},
		{"link-local v4 / aws metadata", "169.254.169.254", false},
		{"link-local v6", "fe80::1", false},
		{"multicast v4", "224.0.0.1", false},
		{"multicast v6", "ff02::1", false},
		{"reserved 240/4", "240.0.0.1", false},
		{"broadcast", "255.255.255.255", false},
		{"TEST-NET-1", "192.0.2.5", false},
		{"TEST-NET-2", "198.51.100.5", false},
		{"TEST-NET-3", "203.0.113.5", false},
		{"benchmark net", "198.18.0.1", false},
		{"v6 ULA fd00/8", "fd00::1", false},
		{"v6 ULA fc00/8", "fc00::1", false},
		{"public v6", "2001:4860:4860::8888", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("parse %q", tc.ip)
			}
			got := isPublicGlobalUnicast(ip)
			if got != tc.pub {
				t.Errorf("isPublicGlobalUnicast(%s) = %v, want %v",
					tc.ip, got, tc.pub)
			}
		})
	}
}

// TestVerifyHTTPRefusesRedirect — a server that 302s the verifier
// should be treated as a failure, not followed. Closes the
// SSRF-via-redirect angle: even with a pinned dialer to the public
// IP, a 3xx response could direct us at an internal URL on the
// redirect re-resolve.
func TestVerifyHTTPRefusesRedirect(t *testing.T) {
	const token = "tcv_test_token_for_redirect_refusal_check"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := net.SplitHostPort(hostPort)
	v := &Verifier{AllowPrivateAddresses: true}
	err := v.VerifyHTTP(context.Background(), host, ":"+port, token)
	if !errors.Is(err, ErrUnexpectedRedirect) {
		t.Errorf("got %v, want ErrUnexpectedRedirect", err)
	}
}

// TestVerifyHTTPRejectsPrivateAddress — with AllowPrivateAddresses
// false (production default), a hostname that resolves to localhost
// is refused before any HTTP call goes out.
func TestVerifyHTTPRejectsPrivateAddress(t *testing.T) {
	v := &Verifier{AllowPrivateAddresses: false}
	err := v.VerifyHTTP(context.Background(), "localhost", ":8080", "irrelevant")
	if !errors.Is(err, ErrAddressNotPublic) {
		t.Errorf("got %v, want ErrAddressNotPublic", err)
	}
}

// TestVerifyHTTPBodyMustMatchToken — server returns 200 with a
// different body; verifier should report ErrTokenMismatch.
func TestVerifyHTTPBodyMustMatchToken(t *testing.T) {
	const token = "tcv_token_under_test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-the-right-token"))
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := net.SplitHostPort(hostPort)
	v := &Verifier{AllowPrivateAddresses: true}
	if err := v.VerifyHTTP(context.Background(), host, ":"+port, token); !errors.Is(err, ErrTokenMismatch) {
		t.Errorf("got %v, want ErrTokenMismatch", err)
	}
}

// TestVerifyHTTPHappyPath — server returns token in body; verifier
// succeeds.
func TestVerifyHTTPHappyPath(t *testing.T) {
	const token = "tcv_happy_path_token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(token))
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := net.SplitHostPort(hostPort)
	v := &Verifier{AllowPrivateAddresses: true}
	if err := v.VerifyHTTP(context.Background(), host, ":"+port, token); err != nil {
		t.Errorf("happy path: %v", err)
	}
}

// TestVerifyHTTPNon200StatusFails — 500 response should not count
// as verification.
func TestVerifyHTTPNon200StatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, port, _ := net.SplitHostPort(hostPort)
	v := &Verifier{AllowPrivateAddresses: true}
	if err := v.VerifyHTTP(context.Background(), host, ":"+port, "tok"); err == nil {
		t.Errorf("500: want error, got nil")
	}
}
