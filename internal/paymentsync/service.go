package paymentsync

import (
	"context"
	"errors"
	"sync"

	"github.com/srmdn/donation/internal/app"
	"github.com/srmdn/donation/internal/payment"
)

type Store interface {
	FindDonationByID(context.Context, int64) (app.Donation, error)
	UpdateDonationProviderStatus(context.Context, int64, string, string, string, string) error
	UpdateDonationProviderObservation(context.Context, int64, string, string, string) error
}

type Client interface {
	Enabled() bool
	Merchant() string
	TransactionDetail(context.Context, string, int) (payment.TransactionStatus, error)
}

type NotifyPaidFunc func(context.Context, app.Donation) error

type Result struct {
	Donation               app.Donation
	ManualProviderConflict bool
	NotificationError      error
}

type Service struct {
	store  Store
	client Client
	notify NotifyPaidFunc
	mu     sync.Mutex
}

func New(store Store, client Client, notify NotifyPaidFunc) *Service {
	return &Service{store: store, client: client, notify: notify}
}

func (s *Service) SyncByID(ctx context.Context, id int64) (Result, error) {
	donation, err := s.store.FindDonationByID(ctx, id)
	if err != nil {
		return Result{}, err
	}
	return s.Sync(ctx, donation)
}

func (s *Service) Sync(ctx context.Context, donation app.Donation) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if donation.ProviderOrderID == "" {
		return Result{Donation: donation}, nil
	}
	if !s.client.Enabled() {
		return Result{}, errors.New("pakasir client is not configured")
	}

	status, err := s.client.TransactionDetail(ctx, donation.ProviderOrderID, donation.Amount)
	if err != nil {
		return Result{}, err
	}
	if status.OrderID != donation.ProviderOrderID {
		return Result{}, errors.New("pakasir order id mismatch")
	}
	if status.Amount != donation.Amount {
		return Result{}, errors.New("pakasir amount mismatch")
	}
	if status.Project != s.client.Merchant() {
		return Result{}, errors.New("pakasir project mismatch")
	}

	if donation.SettlementSource == "manual_transfer" {
		if err := s.store.UpdateDonationProviderObservation(ctx, donation.ID, status.Status, status.PaymentMethod, status.CompletedAt); err != nil {
			return Result{}, err
		}
		updated, err := s.store.FindDonationByID(ctx, donation.ID)
		if err != nil {
			return Result{}, err
		}
		return Result{Donation: updated, ManualProviderConflict: status.Status == "completed"}, nil
	}

	var localStatus string
	switch status.Status {
	case "completed":
		localStatus = "paid"
	case "pending":
		localStatus = "pending_payment"
	case "cancelled", "expired":
		localStatus = status.Status
	default:
		return Result{Donation: donation}, nil
	}
	completedAt := status.CompletedAt
	if status.Status != "completed" {
		completedAt = ""
	}
	if err := s.store.UpdateDonationProviderStatus(ctx, donation.ID, localStatus, status.Status, status.PaymentMethod, completedAt); err != nil {
		return Result{}, err
	}
	updated, err := s.store.FindDonationByID(ctx, donation.ID)
	if err != nil {
		return Result{}, err
	}
	result := Result{Donation: updated}
	if updated.Status == "paid" && updated.SettlementSource == "pakasir" && s.notify != nil {
		result.NotificationError = s.notify(ctx, updated)
	}
	return result, nil
}
