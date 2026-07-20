package paymentsync

import (
	"context"
	"sync"
	"testing"

	"github.com/srmdn/donation/internal/app"
	"github.com/srmdn/donation/internal/payment"
)

type fakeStore struct {
	mu       sync.Mutex
	donation app.Donation
}

func (s *fakeStore) FindDonationByID(context.Context, int64) (app.Donation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.donation, nil
}

func (s *fakeStore) UpdateDonationProviderStatus(_ context.Context, _ int64, status, providerStatus, method, completedAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.donation.Status = status
	s.donation.ProviderStatus = providerStatus
	s.donation.ProviderPaymentMethod = method
	if completedAt != "" {
		s.donation.ProviderCompletedAt = completedAt
	}
	if status == "paid" && s.donation.SettlementSource == "" {
		s.donation.SettlementSource = "pakasir"
	}
	return nil
}

func (s *fakeStore) UpdateDonationProviderObservation(_ context.Context, _ int64, status, method, completedAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.donation.ProviderStatus = status
	s.donation.ProviderPaymentMethod = method
	s.donation.ProviderCompletedAt = completedAt
	return nil
}

type fakeClient struct{ status payment.TransactionStatus }

func (fakeClient) Enabled() bool    { return true }
func (fakeClient) Merchant() string { return "demo" }
func (c fakeClient) TransactionDetail(context.Context, string, int) (payment.TransactionStatus, error) {
	return c.status, nil
}

func baseDonation() app.Donation {
	return app.Donation{ID: 1, Amount: 25000, Status: "pending_payment", Provider: "pakasir", ProviderOrderID: "DON-1"}
}

func TestSyncMapsProviderStatuses(t *testing.T) {
	tests := map[string]string{"completed": "paid", "pending": "pending_payment", "cancelled": "cancelled", "expired": "expired"}
	for providerStatus, localStatus := range tests {
		t.Run(providerStatus, func(t *testing.T) {
			store := &fakeStore{donation: baseDonation()}
			client := fakeClient{status: payment.TransactionStatus{OrderID: "DON-1", Project: "demo", Amount: 25000, Status: providerStatus}}
			service := New(store, client, nil)
			result, err := service.Sync(context.Background(), store.donation)
			if err != nil {
				t.Fatal(err)
			}
			if result.Donation.Status != localStatus {
				t.Fatalf("got %q, want %q", result.Donation.Status, localStatus)
			}
		})
	}
}

func TestSyncRejectsAmountMismatch(t *testing.T) {
	store := &fakeStore{donation: baseDonation()}
	client := fakeClient{status: payment.TransactionStatus{OrderID: "DON-1", Project: "demo", Amount: 1, Status: "completed"}}
	_, err := New(store, client, nil).Sync(context.Background(), store.donation)
	if err == nil || err.Error() != "pakasir amount mismatch" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestManualSettlementPreservesPaidStateAndReportsConflict(t *testing.T) {
	donation := baseDonation()
	donation.Status = "paid"
	donation.SettlementSource = "manual_transfer"
	store := &fakeStore{donation: donation}
	client := fakeClient{status: payment.TransactionStatus{OrderID: "DON-1", Project: "demo", Amount: 25000, Status: "completed", CompletedAt: "2026-07-20T09:14:14Z"}}
	notifyCount := 0
	result, err := New(store, client, func(context.Context, app.Donation) error { notifyCount++; return nil }).Sync(context.Background(), donation)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ManualProviderConflict || result.Donation.SettlementSource != "manual_transfer" || notifyCount != 0 {
		t.Fatalf("unexpected result=%#v notifyCount=%d", result, notifyCount)
	}
}
