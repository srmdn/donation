package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srmdn/donation/internal/app"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "donation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCreateManualDonationDefaultsAreStored(t *testing.T) {
	db := openTestStore(t)
	projects, err := db.ListAllProjects(context.Background())
	if err != nil || len(projects) == 0 {
		t.Fatalf("projects: %v", err)
	}
	before, err := db.FindProject(context.Background(), projects[0].Slug)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateManualDonation(context.Background(), app.ManualDonationInput{
		ProjectID: projects[0].ID, Amount: 15000, PaidAt: "2026-07-20 02:00:00", Visibility: "hidden", ManualReference: "BANK-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	donation, err := db.FindDonationByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if donation.Status != "paid" || donation.Provider != "manual" || donation.SettlementSource != "manual_transfer" || donation.Visibility != "hidden" || donation.ManualReference != "BANK-1" {
		t.Fatalf("unexpected donation: %#v", donation)
	}
	afterHidden, _ := db.FindProject(context.Background(), projects[0].Slug)
	if afterHidden.Raised != before.Raised+15000 {
		t.Fatalf("hidden donation was not counted in accounting total: got %d, want %d", afterHidden.Raised, before.Raised+15000)
	}
	if err := db.UpdateDonationModeration(context.Background(), id, "show_public"); err != nil {
		t.Fatal(err)
	}
	afterPublic, _ := db.FindProject(context.Background(), projects[0].Slug)
	if afterPublic.Raised != before.Raised+15000 {
		t.Fatalf("public toggle changed accounting total=%d, want %d", afterPublic.Raised, before.Raised+15000)
	}
}

func TestMarkDonationManualPaidPreservesPakasirAudit(t *testing.T) {
	db := openTestStore(t)
	id, err := db.CreatePendingDonation(context.Background(), "foliocms", "Donor", "donor@example.com", "", 25000)
	if err != nil {
		t.Fatal(err)
	}
	donation, _ := db.FindDonationByID(context.Background(), id)
	donation.Provider = "pakasir"
	donation.ProviderOrderID = "DON-MANUAL"
	if err := db.UpdateDonationPaymentDraft(context.Background(), donation); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDonationManualPaid(context.Background(), id, "2026-07-20 02:00:00", "BANK-2", "verified", "public"); err != nil {
		t.Fatal(err)
	}
	updated, _ := db.FindDonationByID(context.Background(), id)
	if updated.Provider != "pakasir" || updated.ProviderOrderID != "DON-MANUAL" || updated.SettlementSource != "manual_transfer" || updated.Status != "paid" {
		t.Fatalf("audit data not preserved: %#v", updated)
	}
	if err := db.MarkDonationManualPaid(context.Background(), id, "2026-07-20 02:00:00", "", "", "hidden"); err == nil {
		t.Fatal("expected second conversion to fail")
	}
}

func TestMigrationBackfillsSettlementSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "donation.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`update donations set settlement_source = '' where status = 'paid'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var empty int
	if err := db.db.QueryRow(`select count(*) from donations where status = 'paid' and settlement_source = ''`).Scan(&empty); err != nil {
		t.Fatal(err)
	}
	if empty != 0 {
		t.Fatalf("found %d paid donations without settlement source", empty)
	}
}

func TestListPakasirReconciliationDonationsFiltersTests(t *testing.T) {
	db := openTestStore(t)
	rows, err := db.ListPakasirReconciliationDonations(context.Background(), 200*24*time.Hour, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected seeded pending Pakasir donation")
	}
	for _, donation := range rows {
		if donation.IsTest || donation.IsSpam || donation.Provider != "pakasir" {
			t.Fatalf("unexpected reconciliation row: %#v", donation)
		}
	}
}

func TestCompletedProviderRefreshPreservesPaidAt(t *testing.T) {
	db := openTestStore(t)
	id, err := db.CreatePendingDonation(context.Background(), "foliocms", "Donor", "donor@example.com", "", 25000)
	if err != nil {
		t.Fatal(err)
	}
	donation, _ := db.FindDonationByID(context.Background(), id)
	donation.Provider = "pakasir"
	donation.ProviderOrderID = "DON-IDEMPOTENT"
	if err := db.UpdateDonationPaymentDraft(context.Background(), donation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`update donations set status = 'paid', settlement_source = 'pakasir', paid_at = '2026-07-06 01:39:30' where id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDonationProviderStatus(context.Background(), id, "paid", "completed", "qris", "2026-07-06T01:39:14Z"); err != nil {
		t.Fatal(err)
	}
	var paidAt string
	if err := db.db.QueryRow(`select paid_at from donations where id = ?`, id).Scan(&paidAt); err != nil {
		t.Fatal(err)
	}
	if paidAt != "2026-07-06 01:39:30" {
		t.Fatalf("paid_at changed to %q", paidAt)
	}
}

func TestProjectReportIncludesHiddenIncomeButNotHiddenExpenses(t *testing.T) {
	db := openTestStore(t)
	projects, err := db.ListAllProjects(context.Background())
	if err != nil || len(projects) == 0 {
		t.Fatalf("projects: %v", err)
	}
	project := projects[0]
	if _, err := db.CreateManualDonation(context.Background(), app.ManualDonationInput{
		ProjectID:  project.ID,
		Amount:     42000,
		PaidAt:     "2026-07-20 02:00:00",
		Visibility: "hidden",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProjectExpense(context.Background(), app.ProjectExpenseInput{
		ProjectID:   project.ID,
		Category:    "hosting",
		Description: "VPS",
		Amount:      25000,
		Visibility:  "public",
		IncurredAt:  "2026-07-20 00:00:00",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProjectExpense(context.Background(), app.ProjectExpenseInput{
		ProjectID:   project.ID,
		Category:    "internal",
		Description: "Private admin note",
		Amount:      10000,
		Visibility:  "hidden",
		IncurredAt:  "2026-07-20 00:00:00",
	}); err != nil {
		t.Fatal(err)
	}

	report, err := db.ProjectReport(context.Background(), project.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasPrivateIncome {
		t.Fatal("expected report to flag private income")
	}
	if report.TotalIncome < 42000 {
		t.Fatalf("hidden income missing from report total: %d", report.TotalIncome)
	}
	if report.TotalExpenses != 25000 {
		t.Fatalf("total expenses=%d, want public expense only", report.TotalExpenses)
	}
}

func TestVoidProjectExpenseRemovesItFromPublicReport(t *testing.T) {
	db := openTestStore(t)
	projects, err := db.ListAllProjects(context.Background())
	if err != nil || len(projects) == 0 {
		t.Fatalf("projects: %v", err)
	}
	project := projects[0]
	id, err := db.CreateProjectExpense(context.Background(), app.ProjectExpenseInput{
		ProjectID:   project.ID,
		Category:    "domain",
		Description: "Domain renewal",
		Amount:      120000,
		Visibility:  "public",
		IncurredAt:  "2026-07-20 00:00:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VoidProjectExpense(context.Background(), id, "wrong project"); err != nil {
		t.Fatal(err)
	}
	report, err := db.ProjectReport(context.Background(), project.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalExpenses != 0 {
		t.Fatalf("voided expense still counted: %d", report.TotalExpenses)
	}
}
