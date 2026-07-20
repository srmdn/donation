package payment

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTransactionDetailErrorDoesNotExposeCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()

	const secret = "super-secret-api-key"
	client := PakasirClient{
		BaseURL:      server.URL,
		APIKey:       secret,
		MerchantSlug: "demo",
		HTTPClient:   &http.Client{Timeout: 5 * time.Millisecond},
	}
	_, err := client.TransactionDetail(t.Context(), "DON-1", 25000)
	if err == nil {
		t.Fatal("expected timeout")
	}
	message := err.Error()
	if strings.Contains(message, secret) || strings.Contains(message, "api_key") || strings.Contains(message, server.URL) {
		t.Fatalf("error exposed request details: %q", message)
	}
}

func TestTransactionDetailInvalidURLDoesNotExposeCredential(t *testing.T) {
	const secret = "another-super-secret-api-key"
	client := PakasirClient{BaseURL: ":", APIKey: secret, MerchantSlug: "demo"}
	_, err := client.TransactionDetail(t.Context(), "DON-1", 25000)
	if err == nil {
		t.Fatal("expected invalid URL error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "api_key") {
		t.Fatalf("error exposed credential: %q", err)
	}
}
