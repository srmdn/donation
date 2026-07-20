package main

import (
	"context"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srmdn/donation/internal/app"
	"github.com/srmdn/donation/internal/payment"
	"github.com/srmdn/donation/internal/paymentsync"
	"github.com/srmdn/donation/internal/store"
)

type testPakasirClient struct{ status payment.TransactionStatus }

func (testPakasirClient) Enabled() bool    { return true }
func (testPakasirClient) Merchant() string { return "demo" }
func (c testPakasirClient) TransactionDetail(context.Context, string, int) (payment.TransactionStatus, error) {
	return c.status, nil
}

type countingMailer struct {
	mu    sync.Mutex
	count int
}

func (*countingMailer) SendMagicLink(string, string) error { return nil }
func (m *countingMailer) SendAdminDonationPaid(string, app.Donation, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.count++
	return nil
}
func (*countingMailer) Configured() bool { return true }

func TestDecodePakasirWebhookAcceptsDocumentedPayloadAndExtraFields(t *testing.T) {
	body := `{"amount":22000,"order_id":" INV-1 ","project":"demo","status":"completed","payment_method":"qris","completed_at":"2026-07-20T09:14:14Z","future_field":"ok"}`
	payload, reason, err := decodePakasirWebhook(strings.NewReader(body), "demo")
	if err != nil {
		t.Fatalf("decode webhook: %v (%s)", err, reason)
	}
	if payload.OrderID != "INV-1" || payload.Amount != 22000 || payload.Project != "demo" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestDecodePakasirWebhookRejectsInvalidPayloads(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		merchant string
		reason   string
	}{
		{"malformed", `{`, "demo", "invalid json"},
		{"multiple objects", `{"amount":1,"order_id":"x","project":"demo"}{}`, "demo", "invalid json"},
		{"missing fields", `{"amount":1,"project":"demo"}`, "demo", "missing required webhook fields"},
		{"project mismatch", `{"amount":1,"order_id":"x","project":"other"}`, "demo", "project mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, reason, err := decodePakasirWebhook(strings.NewReader(test.body), test.merchant)
			if err == nil || reason != test.reason {
				t.Fatalf("got error=%v reason=%q, want %q", err, reason, test.reason)
			}
		})
	}
}

func TestManualDonationInputDefaultsHidden(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, jakartaLocation())
	form := url.Values{
		"project_id": {"1"},
		"amount":     {"15000"},
		"paid_at":    {now.Format("2006-01-02T15:04")},
		"email":      {"donor@example.com"},
	}
	request := httptest.NewRequest("POST", "/admin/donations/manual", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	input, err := manualDonationInputFromRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if input.Visibility != "hidden" || input.Amount != 15000 {
		t.Fatalf("unexpected input: %#v", input)
	}
}

func TestManualPaidAtRejectsFuture(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, jakartaLocation())
	if _, err := manualPaidAtFromRequest("2026-07-20T10:02", now); err == nil {
		t.Fatal("expected future paid time to fail")
	}
	value, err := manualPaidAtFromRequest("2026-07-20T09:59", now)
	if err != nil || value != "2026-07-20 02:59:00" {
		t.Fatalf("got value=%q error=%v", value, err)
	}
}

func TestReconcileIntervalCanBeDisabled(t *testing.T) {
	t.Setenv("PAYMENT_RECONCILE_INTERVAL", "0")
	if got := envDuration("PAYMENT_RECONCILE_INTERVAL", 5*time.Minute); got != 0 {
		t.Fatalf("got %s, want disabled interval", got)
	}
}

func TestConcurrentPaymentSyncSendsOneAdminEmail(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "donation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.CreatePendingDonation(context.Background(), "foliocms", "Donor", "donor@example.com", "", 25000)
	if err != nil {
		t.Fatal(err)
	}
	donation, _ := db.FindDonationByID(context.Background(), id)
	donation.Provider = "pakasir"
	donation.ProviderOrderID = "DON-CONCURRENT"
	if err := db.UpdateDonationPaymentDraft(context.Background(), donation); err != nil {
		t.Fatal(err)
	}
	donation, _ = db.FindDonationByID(context.Background(), id)
	mailer := &countingMailer{}
	client := testPakasirClient{status: payment.TransactionStatus{
		OrderID: "DON-CONCURRENT", Project: "demo", Amount: 25000, Status: "completed",
	}}
	service := paymentsync.New(db, client, func(ctx context.Context, updated app.Donation) error {
		return notifyAdminDonationPaid(ctx, db, mailer, "admin@example.com", "https://donate.example", updated)
	})

	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := service.Sync(context.Background(), donation); err != nil {
				t.Errorf("sync: %v", err)
			}
		}()
	}
	wait.Wait()
	mailer.mu.Lock()
	defer mailer.mu.Unlock()
	if mailer.count != 1 {
		t.Fatalf("sent %d emails, want 1", mailer.count)
	}
}
