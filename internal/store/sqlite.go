package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srmdn/donation/internal/app"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func ErrNotFound() error {
	return sql.ErrNoRows
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.seed(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) PageData(ctx context.Context) (app.PageData, error) {
	return s.PageDataWithTimelineLimit(ctx, 6)
}

func (s *Store) PageDataWithTimelineLimit(ctx context.Context, limit int) (app.PageData, error) {
	builder := app.DefaultBuilder()

	projects, err := s.ListFeaturedProjects(ctx, 6)
	if err != nil {
		return app.PageData{}, err
	}

	timeline, hasMore, err := s.ListTimeline(ctx, "", limit)
	if err != nil {
		return app.PageData{}, err
	}

	total := 0
	for _, project := range projects {
		total += project.Raised
	}

	var supporters int
	if err := s.db.QueryRowContext(ctx, `select count(*) from donations where status = 'paid' and is_spam = 0 and is_test = 0`).Scan(&supporters); err != nil {
		return app.PageData{}, err
	}
	activeProjects, err := s.CountActiveProjects(ctx)
	if err != nil {
		return app.PageData{}, err
	}

	return app.PageData{
		Builder:           builder,
		TotalRaised:       total,
		SupporterCount:    supporters,
		ActiveProjectNum:  activeProjects,
		Projects:          projects,
		Timeline:          timeline,
		TimelineHasMore:   hasMore,
		TimelineNextLimit: limit + 6,
	}, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]app.Project, error) {
	return s.listProjects(ctx, true)
}

func (s *Store) ListFeaturedProjects(ctx context.Context, limit int) ([]app.Project, error) {
	if limit <= 0 {
		limit = 6
	}
	return s.listProjectsWithLimit(ctx, true, limit, 0)
}

func (s *Store) ListProjectsPage(ctx context.Context, limit, offset int) ([]app.Project, bool, error) {
	if limit <= 0 {
		limit = 12
	}
	if offset < 0 {
		offset = 0
	}

	projects, err := s.listProjectsWithLimit(ctx, true, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	hasNext := len(projects) > limit
	if hasNext {
		projects = projects[:limit]
	}
	return projects, hasNext, nil
}

func (s *Store) ListAllProjects(ctx context.Context) ([]app.Project, error) {
	return s.listProjects(ctx, false)
}

func (s *Store) listProjects(ctx context.Context, activeOnly bool) ([]app.Project, error) {
	return s.listProjectsWithLimit(ctx, activeOnly, 0, 0)
}

func (s *Store) listProjectsWithLimit(ctx context.Context, activeOnly bool, limit, offset int) ([]app.Project, error) {
	where := ""
	if activeOnly {
		where = "where p.is_active = 1"
	}
	limitClause := ""
	args := []any{}
	if limit > 0 {
		limitClause = " limit ? offset ?"
		args = append(args, limit, offset)
	}

	rows, err := s.db.QueryContext(ctx, `
		select
			p.id,
			p.title,
			p.slug,
			p.summary,
			p.description,
			p.status,
			p.goal_amount,
			p.accent,
			p.repo_url,
			p.demo_url,
			coalesce(p.deadline_date, ''),
			p.is_active,
			coalesce(sum(case when d.status = 'paid' and d.is_spam = 0 and d.is_test = 0 then d.amount else 0 end), 0) as raised,
			max(coalesce(max(u.published_at), ''), p.updated_at) as last_updated
		from projects p
		left join donations d on d.project_id = p.id
		left join project_updates u on u.project_id = p.id
		`+where+`
		group by p.id
		order by p.id asc
	`+limitClause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []app.Project
	for rows.Next() {
		var project app.Project
		var updatedAt string
		if err := rows.Scan(
			&project.ID,
			&project.Title,
			&project.Slug,
			&project.Summary,
			&project.Description,
			&project.Status,
			&project.Goal,
			&project.Accent,
			&project.RepoURL,
			&project.DemoURL,
			&project.DeadlineDate,
			&project.IsActive,
			&project.Raised,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		project.LastUpdated = relativeTime(updatedAt)
		project.DeadlineText, project.DeadlineEnded = app.DeadlineStatus(project.DeadlineDate, time.Now())
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func (s *Store) CountActiveProjects(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `select count(*) from projects where is_active = 1`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) FindProject(ctx context.Context, slug string) (app.Project, error) {
	projects, err := s.ListAllProjects(ctx)
	if err != nil {
		return app.Project{}, err
	}
	for _, project := range projects {
		if project.Slug == slug {
			return project, nil
		}
	}
	return app.Project{}, sql.ErrNoRows
}

func (s *Store) FindProjectByID(ctx context.Context, id int64) (app.Project, error) {
	projects, err := s.ListAllProjects(ctx)
	if err != nil {
		return app.Project{}, err
	}
	for _, project := range projects {
		if project.ID == id {
			return project, nil
		}
	}
	return app.Project{}, sql.ErrNoRows
}

func (s *Store) ListTimeline(ctx context.Context, projectSlug string, limit int) ([]app.TimelineEvent, bool, error) {
	if limit <= 0 {
		limit = 6
	}

	args := []any{}
	filter := ""
	if projectSlug != "" {
		filter = "where slug = ?"
		args = append(args, projectSlug)
	}
	args = append(args, limit+1)

	query := fmt.Sprintf(`
		select kind, title, detail, amount, project, occurred_at
		from (
			select
				'donation' as kind,
				case
					when donor_name = '' then 'Orang Baik mendukung ' || p.title
					else donor_name || ' mendukung ' || p.title
				end as title,
				coalesce(nullif(message, ''), 'Tanpa pesan publik.') as detail,
				d.amount as amount,
				p.title as project,
				d.paid_at as occurred_at,
				p.slug as slug
			from donations d
			join projects p on p.id = d.project_id
			where d.status = 'paid' and d.visibility = 'public' and d.is_spam = 0 and d.is_test = 0
			union all
			select
				'update' as kind,
				u.title,
				u.body as detail,
				0 as amount,
				p.title as project,
				u.published_at as occurred_at,
				p.slug as slug
			from project_updates u
			join projects p on p.id = u.project_id
		)
		%s
		order by occurred_at desc
		limit ?
	`, filter)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var events []app.TimelineEvent
	for rows.Next() {
		var event app.TimelineEvent
		var occurredAt string
		if err := rows.Scan(&event.Kind, &event.Title, &event.Detail, &event.Amount, &event.Project, &occurredAt); err != nil {
			return nil, false, err
		}
		event.TimeAgo = relativeTime(occurredAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}

	return events, hasMore, nil
}

func (s *Store) ProjectReport(ctx context.Context, slug string) (app.ProjectReportPageData, error) {
	project, err := s.FindProject(ctx, slug)
	if err != nil {
		return app.ProjectReportPageData{}, err
	}

	income, totalIncome, hasPrivateIncome, err := s.ListProjectReportIncome(ctx, project.ID)
	if err != nil {
		return app.ProjectReportPageData{}, err
	}
	expenses, totalExpenses, err := s.ListProjectReportExpenses(ctx, project.ID)
	if err != nil {
		return app.ProjectReportPageData{}, err
	}

	return app.ProjectReportPageData{
		Builder:          app.DefaultBuilder(),
		Project:          project,
		Income:           income,
		Expenses:         expenses,
		TotalIncome:      totalIncome,
		TotalExpenses:    totalExpenses,
		Balance:          totalIncome - totalExpenses,
		DonationCount:    len(income),
		ExpenseCount:     len(expenses),
		HasPrivateIncome: hasPrivateIncome,
	}, nil
}

func (s *Store) ListProjectReportIncome(ctx context.Context, projectID int64) ([]app.ReportIncomeEntry, int, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		select
			id,
			case when visibility = 'public' and donor_name != '' then donor_name else 'Orang Baik' end,
			amount,
			visibility,
			settlement_source,
			coalesce(paid_at, created_at)
		from donations
		where project_id = ?
			and status = 'paid'
			and is_spam = 0
			and is_test = 0
		order by coalesce(paid_at, created_at) desc, id desc
	`, projectID)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()

	var entries []app.ReportIncomeEntry
	total := 0
	hasPrivate := false
	for rows.Next() {
		var entry app.ReportIncomeEntry
		var paidAt string
		if err := rows.Scan(&entry.ID, &entry.DonorName, &entry.Amount, &entry.Visibility, &entry.SettlementSource, &paidAt); err != nil {
			return nil, 0, false, err
		}
		entry.PaidAt = displayJakartaTime(paidAt)
		if entry.Visibility != "public" {
			hasPrivate = true
		}
		total += entry.Amount
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	return entries, total, hasPrivate, nil
}

func (s *Store) ListProjectReportExpenses(ctx context.Context, projectID int64) ([]app.ProjectExpense, int, error) {
	rows, err := s.db.QueryContext(ctx, `
		select
			e.id,
			e.project_id,
			p.slug,
			p.title,
			e.category,
			e.description,
			e.vendor,
			e.reference,
			e.amount,
			e.currency,
			e.visibility,
			e.is_voided,
			coalesce(e.voided_at, ''),
			e.moderation_note,
			e.incurred_at,
			e.created_at,
			e.updated_at
		from project_expenses e
		join projects p on p.id = e.project_id
		where e.project_id = ?
			and e.visibility = 'public'
			and e.is_voided = 0
		order by e.incurred_at desc, e.id desc
	`, projectID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var expenses []app.ProjectExpense
	total := 0
	for rows.Next() {
		expense, err := scanExpense(rows)
		if err != nil {
			return nil, 0, err
		}
		total += expense.Amount
		expenses = append(expenses, expense)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return expenses, total, nil
}

func (s *Store) CreatePendingDonation(ctx context.Context, projectSlug, name, email, message string, amount int) (int64, error) {
	if amount <= 0 {
		return 0, errors.New("amount must be positive")
	}

	var projectID int64
	if err := s.db.QueryRowContext(ctx, `select id from projects where slug = ?`, projectSlug).Scan(&projectID); err != nil {
		return 0, err
	}

	result, err := s.db.ExecContext(ctx, `
		insert into donations (
			project_id, donor_name, donor_email, message, amount, currency,
			status, visibility, is_spam, moderation_note,
			provider, provider_status,
			created_at, updated_at
		)
		values (?, ?, ?, ?, ?, 'IDR', 'pending_payment', 'public', 0, '', 'mock', 'pending', datetime('now'), datetime('now'))
	`, projectID, name, email, message, amount)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) MarkDonationPaid(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		update donations
		set
			status = 'paid',
			provider_status = 'completed',
			provider_completed_at = datetime('now'),
			settlement_source = 'mock',
			paid_at = datetime('now'),
			updated_at = datetime('now')
		where id = ?
	`, id)
	return err
}

func (s *Store) CreateManualDonation(ctx context.Context, input app.ManualDonationInput) (int64, error) {
	if input.ProjectID <= 0 {
		return 0, errors.New("project is required")
	}
	if input.Amount <= 0 {
		return 0, errors.New("amount must be positive")
	}
	if input.Visibility != "hidden" && input.Visibility != "public" {
		return 0, errors.New("invalid visibility")
	}

	result, err := s.db.ExecContext(ctx, `
		insert into donations (
			project_id, donor_name, donor_email, message, amount, currency,
			status, visibility, is_spam, is_test, moderation_note,
			provider, provider_status, settlement_source, manual_reference,
			provider_completed_at, paid_at, created_at, updated_at
		)
		select
			id, ?, ?, ?, ?, 'IDR',
			'paid', ?, 0, 0, ?,
			'manual', 'completed', 'manual_transfer', ?,
			?, ?, datetime('now'), datetime('now')
		from projects
		where id = ?
	`, input.DonorName, input.DonorEmail, input.Message, input.Amount, input.Visibility,
		input.ModerationNote, input.ManualReference, input.PaidAt, input.PaidAt, input.ProjectID)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		return 0, sql.ErrNoRows
	}
	return result.LastInsertId()
}

func (s *Store) MarkDonationManualPaid(ctx context.Context, id int64, paidAt, reference, note, visibility string) error {
	if visibility != "hidden" && visibility != "public" {
		return errors.New("invalid visibility")
	}
	result, err := s.db.ExecContext(ctx, `
		update donations
		set
			status = 'paid',
			visibility = ?,
			settlement_source = 'manual_transfer',
			manual_reference = ?,
			moderation_note = ?,
			paid_at = ?,
			updated_at = datetime('now')
		where id = ? and status = 'pending_payment' and provider = 'pakasir'
	`, visibility, strings.TrimSpace(reference), strings.TrimSpace(note), paidAt, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) FindDonationByID(ctx context.Context, id int64) (app.Donation, error) {
	row := s.db.QueryRowContext(ctx, `
		select
			d.id,
			d.project_id,
			p.title,
			p.slug,
			d.donor_name,
			d.donor_email,
			d.message,
			d.amount,
			d.currency,
			d.status,
			d.visibility,
			d.is_spam,
			d.is_test,
			d.moderation_note,
			d.provider,
			d.provider_order_id,
			d.provider_status,
			d.provider_payment_url,
			d.provider_payment_method,
			d.provider_payment_number,
			d.provider_fee,
			d.provider_total_payment,
			coalesce(d.provider_expired_at, ''),
			coalesce(d.provider_completed_at, ''),
			d.settlement_source,
			d.manual_reference,
			coalesce(d.paid_at, ''),
			d.created_at,
			d.updated_at
		from donations d
		join projects p on p.id = d.project_id
		where d.id = ?
	`, id)

	return scanDonation(row)
}

func (s *Store) FindDonationByOrderID(ctx context.Context, orderID string) (app.Donation, error) {
	row := s.db.QueryRowContext(ctx, `
		select
			d.id,
			d.project_id,
			p.title,
			p.slug,
			d.donor_name,
			d.donor_email,
			d.message,
			d.amount,
			d.currency,
			d.status,
			d.visibility,
			d.is_spam,
			d.is_test,
			d.moderation_note,
			d.provider,
			d.provider_order_id,
			d.provider_status,
			d.provider_payment_url,
			d.provider_payment_method,
			d.provider_payment_number,
			d.provider_fee,
			d.provider_total_payment,
			coalesce(d.provider_expired_at, ''),
			coalesce(d.provider_completed_at, ''),
			d.settlement_source,
			d.manual_reference,
			coalesce(d.paid_at, ''),
			d.created_at,
			d.updated_at
		from donations d
		join projects p on p.id = d.project_id
		where d.provider_order_id = ?
	`, orderID)

	return scanDonation(row)
}

func (s *Store) UpdateDonationPaymentDraft(ctx context.Context, donation app.Donation) error {
	result, err := s.db.ExecContext(ctx, `
		update donations
		set
			provider = ?,
			provider_order_id = ?,
			provider_status = ?,
			provider_payment_url = ?,
			provider_payment_method = ?,
			provider_payment_number = ?,
			provider_fee = ?,
			provider_total_payment = ?,
			provider_expired_at = ?,
			updated_at = datetime('now')
		where id = ?
	`, donation.Provider, donation.ProviderOrderID, donation.ProviderStatus, donation.ProviderPaymentURL, donation.ProviderPaymentMethod, donation.ProviderPaymentNumber, donation.ProviderFee, donation.ProviderTotalPayment, nullIfEmpty(donation.ProviderExpiredAt), donation.ID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpdateDonationProviderStatus(ctx context.Context, id int64, status, providerStatus, paymentMethod, completedAt string) error {
	_, err := s.db.ExecContext(ctx, `
		update donations
		set
			status = ?,
			provider_status = ?,
			provider_payment_method = case when ? = '' then provider_payment_method else ? end,
			provider_completed_at = case when ? = '' then provider_completed_at else ? end,
			settlement_source = case when ? = 'paid' and settlement_source = '' then 'pakasir' else settlement_source end,
			paid_at = case when ? = 'paid' and paid_at is null then datetime('now') else paid_at end,
			updated_at = datetime('now')
		where id = ?
	`, status, providerStatus, paymentMethod, paymentMethod, completedAt, completedAt, status, status, id)
	return err
}

func (s *Store) UpdateDonationProviderObservation(ctx context.Context, id int64, providerStatus, paymentMethod, completedAt string) error {
	_, err := s.db.ExecContext(ctx, `
		update donations
		set
			provider_status = ?,
			provider_payment_method = case when ? = '' then provider_payment_method else ? end,
			provider_completed_at = case when ? = '' then provider_completed_at else ? end,
			updated_at = datetime('now')
		where id = ?
	`, providerStatus, paymentMethod, paymentMethod, completedAt, completedAt, id)
	return err
}

func (s *Store) ListPakasirReconciliationDonations(ctx context.Context, lookback time.Duration, limit int) ([]app.Donation, error) {
	if limit <= 0 {
		limit = 25
	}
	if lookback <= 0 {
		lookback = 48 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-lookback).Format("2006-01-02 15:04:05")
	rows, err := s.db.QueryContext(ctx, `
		select
			d.id, d.project_id, p.title, p.slug, d.donor_name, d.donor_email,
			d.message, d.amount, d.currency, d.status, d.visibility, d.is_spam,
			d.is_test, d.moderation_note, d.provider, d.provider_order_id,
			d.provider_status, d.provider_payment_url, d.provider_payment_method,
			d.provider_payment_number, d.provider_fee, d.provider_total_payment,
			coalesce(d.provider_expired_at, ''), coalesce(d.provider_completed_at, ''),
			d.settlement_source, d.manual_reference, coalesce(d.paid_at, ''),
			d.created_at, d.updated_at
		from donations d
		join projects p on p.id = d.project_id
		where d.provider = 'pakasir'
			and d.provider_order_id != ''
			and d.is_spam = 0
			and d.is_test = 0
			and d.created_at >= ?
			and (
				d.status = 'pending_payment'
				or (d.settlement_source = 'manual_transfer' and d.provider_status in ('', 'pending'))
				or (d.status = 'paid' and d.settlement_source = 'pakasir' and d.admin_notified_at is null)
			)
		order by d.created_at asc, d.id asc
		limit ?
	`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var donations []app.Donation
	for rows.Next() {
		donation, err := scanDonation(rows)
		if err != nil {
			return nil, err
		}
		donations = append(donations, donation)
	}
	return donations, rows.Err()
}

func (s *Store) DonationAdminNotified(ctx context.Context, id int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		select 1
		from donations
		where id = ? and admin_notified_at is not null
	`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) MarkDonationAdminNotified(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		update donations
		set admin_notified_at = datetime('now'), updated_at = datetime('now')
		where id = ?
	`, id)
	return err
}

func (s *Store) ListAdminDonations(ctx context.Context, limit int, status, visibility, spam, testFlag, projectSlug, search string) ([]app.Donation, error) {
	if limit <= 0 {
		limit = 100
	}

	where := []string{"1 = 1"}
	args := []any{}

	if status != "" {
		where = append(where, "d.status = ?")
		args = append(args, status)
	}
	if visibility != "" {
		where = append(where, "d.visibility = ?")
		args = append(args, visibility)
	}
	if spam == "spam" {
		where = append(where, "d.is_spam = 1")
	}
	if spam == "clean" {
		where = append(where, "d.is_spam = 0")
	}
	if testFlag == "test" {
		where = append(where, "d.is_test = 1")
	}
	if testFlag == "live" {
		where = append(where, "d.is_test = 0")
	}
	if projectSlug != "" {
		where = append(where, "p.slug = ?")
		args = append(args, projectSlug)
	}
	search = strings.TrimSpace(strings.ToLower(search))
	if search != "" {
		where = append(where, `(lower(d.donor_name) like ? or lower(d.donor_email) like ? or lower(d.provider_order_id) like ? or lower(p.title) like ?)`)
		like := "%" + search + "%"
		args = append(args, like, like, like, like)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		select
			d.id,
			d.project_id,
			p.title,
			p.slug,
			d.donor_name,
			d.donor_email,
			d.message,
			d.amount,
			d.currency,
			d.status,
			d.visibility,
			d.is_spam,
			d.is_test,
			d.moderation_note,
			d.provider,
			d.provider_order_id,
			d.provider_status,
			d.provider_payment_url,
			d.provider_payment_method,
			d.provider_payment_number,
			d.provider_fee,
			d.provider_total_payment,
			coalesce(d.provider_expired_at, ''),
			coalesce(d.provider_completed_at, ''),
			d.settlement_source,
			d.manual_reference,
			coalesce(d.paid_at, ''),
			d.created_at,
			d.updated_at
		from donations d
		join projects p on p.id = d.project_id
		where %s
		order by d.created_at desc, d.id desc
		limit ?
	`, strings.Join(where, " and ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var donations []app.Donation
	for rows.Next() {
		donation, err := scanDonation(rows)
		if err != nil {
			return nil, err
		}
		donations = append(donations, donation)
	}

	return donations, rows.Err()
}

func (s *Store) CreateProjectExpense(ctx context.Context, input app.ProjectExpenseInput) (int64, error) {
	if input.ProjectID <= 0 {
		return 0, errors.New("project is required")
	}
	if input.Amount <= 0 {
		return 0, errors.New("amount must be positive")
	}
	if input.Category == "" {
		return 0, errors.New("category is required")
	}
	if input.Description == "" {
		return 0, errors.New("description is required")
	}
	if input.Visibility != "hidden" && input.Visibility != "public" {
		return 0, errors.New("invalid visibility")
	}

	result, err := s.db.ExecContext(ctx, `
		insert into project_expenses (
			project_id, category, description, vendor, reference, amount, currency,
			visibility, is_voided, moderation_note, incurred_at, created_at, updated_at
		)
		select
			id, ?, ?, ?, ?, ?, 'IDR',
			?, 0, ?, ?, datetime('now'), datetime('now')
		from projects
		where id = ?
	`, input.Category, input.Description, input.Vendor, input.Reference, input.Amount,
		input.Visibility, input.ModerationNote, input.IncurredAt, input.ProjectID)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		return 0, sql.ErrNoRows
	}
	return result.LastInsertId()
}

func (s *Store) VoidProjectExpense(ctx context.Context, id int64, note string) error {
	result, err := s.db.ExecContext(ctx, `
		update project_expenses
		set is_voided = 1,
			visibility = 'hidden',
			moderation_note = ?,
			voided_at = datetime('now'),
			updated_at = datetime('now')
		where id = ? and is_voided = 0
	`, strings.TrimSpace(note), id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListAdminProjectExpenses(ctx context.Context, limit int) ([]app.ProjectExpense, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		select
			e.id,
			e.project_id,
			p.slug,
			p.title,
			e.category,
			e.description,
			e.vendor,
			e.reference,
			e.amount,
			e.currency,
			e.visibility,
			e.is_voided,
			coalesce(e.voided_at, ''),
			e.moderation_note,
			e.incurred_at,
			e.created_at,
			e.updated_at
		from project_expenses e
		join projects p on p.id = e.project_id
		order by e.incurred_at desc, e.id desc
		limit ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var expenses []app.ProjectExpense
	for rows.Next() {
		expense, err := scanExpense(rows)
		if err != nil {
			return nil, err
		}
		expenses = append(expenses, expense)
	}
	return expenses, rows.Err()
}

func scanExpense(scanner interface {
	Scan(dest ...any) error
}) (app.ProjectExpense, error) {
	var expense app.ProjectExpense
	var isVoided int
	if err := scanner.Scan(
		&expense.ID,
		&expense.ProjectID,
		&expense.ProjectSlug,
		&expense.ProjectTitle,
		&expense.Category,
		&expense.Description,
		&expense.Vendor,
		&expense.Reference,
		&expense.Amount,
		&expense.Currency,
		&expense.Visibility,
		&isVoided,
		&expense.VoidedAt,
		&expense.ModerationNote,
		&expense.IncurredAt,
		&expense.CreatedAt,
		&expense.UpdatedAt,
	); err != nil {
		return app.ProjectExpense{}, err
	}
	expense.IsVoided = isVoided == 1
	expense.IncurredAt = displayJakartaDate(expense.IncurredAt)
	expense.CreatedAt = displayJakartaTime(expense.CreatedAt)
	expense.UpdatedAt = displayJakartaTime(expense.UpdatedAt)
	expense.VoidedAt = displayJakartaTime(expense.VoidedAt)
	return expense, nil
}

func scanDonation(scanner interface {
	Scan(dest ...any) error
}) (app.Donation, error) {
	var donation app.Donation
	var isSpam int
	var isTest int
	if err := scanner.Scan(
		&donation.ID,
		&donation.ProjectID,
		&donation.ProjectTitle,
		&donation.ProjectSlug,
		&donation.DonorName,
		&donation.DonorEmail,
		&donation.Message,
		&donation.Amount,
		&donation.Currency,
		&donation.Status,
		&donation.Visibility,
		&isSpam,
		&isTest,
		&donation.ModerationNote,
		&donation.Provider,
		&donation.ProviderOrderID,
		&donation.ProviderStatus,
		&donation.ProviderPaymentURL,
		&donation.ProviderPaymentMethod,
		&donation.ProviderPaymentNumber,
		&donation.ProviderFee,
		&donation.ProviderTotalPayment,
		&donation.ProviderExpiredAt,
		&donation.ProviderCompletedAt,
		&donation.SettlementSource,
		&donation.ManualReference,
		&donation.PaidAt,
		&donation.CreatedAt,
		&donation.UpdatedAt,
	); err != nil {
		return app.Donation{}, err
	}
	donation.IsSpam = isSpam == 1
	donation.IsTest = isTest == 1
	donation.CreatedAt = displayJakartaTime(donation.CreatedAt)
	donation.UpdatedAt = displayJakartaTime(donation.UpdatedAt)
	donation.PaidAt = displayJakartaTime(donation.PaidAt)
	donation.ProviderExpiredAt = displayJakartaTime(donation.ProviderExpiredAt)
	donation.ProviderCompletedAt = displayJakartaTime(donation.ProviderCompletedAt)
	return donation, nil
}

func (s *Store) UpdateDonationModeration(ctx context.Context, id int64, action string) error {
	var (
		query string
		args  []any
	)

	switch action {
	case "hide_public":
		query = `update donations set visibility = 'hidden', updated_at = datetime('now') where id = ?`
		args = []any{id}
	case "show_public":
		query = `update donations set visibility = 'public', updated_at = datetime('now') where id = ?`
		args = []any{id}
	case "mark_test":
		query = `update donations set is_test = 1, visibility = 'hidden', updated_at = datetime('now') where id = ?`
		args = []any{id}
	case "unmark_test":
		query = `update donations set is_test = 0, updated_at = datetime('now') where id = ?`
		args = []any{id}
	case "mark_spam":
		query = `update donations set is_spam = 1, updated_at = datetime('now') where id = ?`
		args = []any{id}
	case "unmark_spam":
		query = `update donations set is_spam = 0, updated_at = datetime('now') where id = ?`
		args = []any{id}
	default:
		return errors.New("invalid moderation action")
	}

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpdateDonationModerationNote(ctx context.Context, id int64, note string) error {
	result, err := s.db.ExecContext(ctx, `
		update donations
		set moderation_note = ?, updated_at = datetime('now')
		where id = ?
	`, strings.TrimSpace(note), id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CreateProject(ctx context.Context, project app.Project) error {
	_, err := s.db.ExecContext(ctx, `
		insert into projects (title, slug, summary, description, status, goal_amount, accent, repo_url, demo_url, deadline_date, is_active, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	`, project.Title, project.Slug, project.Summary, project.Description, project.Status, project.Goal, project.Accent, project.RepoURL, project.DemoURL, nullIfEmpty(project.DeadlineDate), boolToInt(project.IsActive))
	return err
}

func (s *Store) UpdateProject(ctx context.Context, project app.Project) error {
	result, err := s.db.ExecContext(ctx, `
		update projects
		set title = ?, slug = ?, summary = ?, description = ?, status = ?, goal_amount = ?, accent = ?, repo_url = ?, demo_url = ?, deadline_date = ?, is_active = ?, updated_at = datetime('now')
		where id = ?
	`, project.Title, project.Slug, project.Summary, project.Description, project.Status, project.Goal, project.Accent, project.RepoURL, project.DemoURL, nullIfEmpty(project.DeadlineDate), boolToInt(project.IsActive), project.ID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CreateProjectUpdate(ctx context.Context, projectID int64, title, body string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into project_updates (project_id, title, body, published_at, created_at, updated_at)
		values (?, ?, ?, datetime('now'), datetime('now'), datetime('now'))
	`, projectID, strings.TrimSpace(title), strings.TrimSpace(body))
	return err
}

func (s *Store) FindProjectUpdateByID(ctx context.Context, id int64) (app.ProjectUpdate, error) {
	row := s.db.QueryRowContext(ctx, `
		select
			u.id,
			u.project_id,
			p.slug,
			p.title,
			u.title,
			u.body,
			u.published_at
		from project_updates u
		join projects p on p.id = u.project_id
		where u.id = ?
	`, id)

	var update app.ProjectUpdate
	err := row.Scan(
		&update.ID,
		&update.ProjectID,
		&update.ProjectSlug,
		&update.ProjectTitle,
		&update.Title,
		&update.Body,
		&update.PublishedAt,
	)
	if err != nil {
		return app.ProjectUpdate{}, err
	}
	return update, nil
}

func (s *Store) UpdateProjectUpdate(ctx context.Context, id, projectID int64, title, body string) error {
	result, err := s.db.ExecContext(ctx, `
		update project_updates
		set project_id = ?, title = ?, body = ?, updated_at = datetime('now')
		where id = ?
	`, projectID, strings.TrimSpace(title), strings.TrimSpace(body), id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteProjectUpdate(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `delete from project_updates where id = ?`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListAdminProjectUpdates(ctx context.Context, limit int) ([]app.ProjectUpdate, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.QueryContext(ctx, `
		select
			u.id,
			u.project_id,
			p.slug,
			p.title,
			u.title,
			u.body,
			u.published_at
		from project_updates u
		join projects p on p.id = u.project_id
		order by u.published_at desc, u.id desc
		limit ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var updates []app.ProjectUpdate
	for rows.Next() {
		var update app.ProjectUpdate
		if err := rows.Scan(
			&update.ID,
			&update.ProjectID,
			&update.ProjectSlug,
			&update.ProjectTitle,
			&update.Title,
			&update.Body,
			&update.PublishedAt,
		); err != nil {
			return nil, err
		}
		update.PublishedAt = displayJakartaTime(update.PublishedAt)
		updates = append(updates, update)
	}
	return updates, rows.Err()
}

var ErrInvalidAdminLoginToken = errors.New("invalid admin login token")

func ErrInvalidAdminLoginTokenError() error {
	return ErrInvalidAdminLoginToken
}

func (s *Store) CreateAdminLoginToken(ctx context.Context, email, token string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		insert into admin_login_tokens (email, token_hash, expires_at, created_at)
		values (?, ?, ?, datetime('now'))
	`, email, hashLoginToken(token), expiresAt.Format("2006-01-02 15:04:05"))
	return err
}

func (s *Store) CreateAdminSession(ctx context.Context, token string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		insert into admin_sessions (token_hash, expires_at, created_at)
		values (?, ?, datetime('now'))
	`, hashLoginToken(token), expiresAt.Format("2006-01-02 15:04:05"))
	return err
}

func (s *Store) HasActiveAdminSession(ctx context.Context, token string, now time.Time) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		select 1
		from admin_sessions
		where token_hash = ? and expires_at > ?
	`, hashLoginToken(token), now.Format("2006-01-02 15:04:05")).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `
		delete from admin_sessions
		where token_hash = ?
	`, hashLoginToken(token))
	return err
}

func (s *Store) PeekAdminLoginToken(ctx context.Context, token string, now time.Time) (string, error) {
	var email string
	err := s.db.QueryRowContext(ctx, `
		select email
		from admin_login_tokens
		where token_hash = ? and used_at is null and expires_at > ?
	`, hashLoginToken(token), now.Format("2006-01-02 15:04:05")).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidAdminLoginToken
	}
	if err != nil {
		return "", err
	}
	return email, nil
}

func (s *Store) ConsumeAdminLoginToken(ctx context.Context, token string, now time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var tokenID int64
	var email string
	err = tx.QueryRowContext(ctx, `
		select id, email
		from admin_login_tokens
		where token_hash = ? and used_at is null and expires_at > ?
	`, hashLoginToken(token), now.Format("2006-01-02 15:04:05")).Scan(&tokenID, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidAdminLoginToken
	}
	if err != nil {
		return "", err
	}

	result, err := tx.ExecContext(ctx, `
		update admin_login_tokens
		set used_at = ?
		where id = ? and used_at is null
	`, now.Format("2006-01-02 15:04:05"), tokenID)
	if err != nil {
		return "", err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if rows != 1 {
		return "", ErrInvalidAdminLoginToken
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return email, nil
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		create table if not exists projects (
			id integer primary key autoincrement,
			title text not null,
			slug text not null unique,
			summary text not null,
			description text not null,
			status text not null,
			goal_amount integer not null,
			accent text not null,
			repo_url text not null default '',
			demo_url text not null default '',
			deadline_date text,
			is_active integer not null default 1,
			created_at text not null,
			updated_at text not null
		);

		create table if not exists donations (
			id integer primary key autoincrement,
			project_id integer not null references projects(id),
			donor_name text not null default '',
			donor_email text not null default '',
			message text not null default '',
			amount integer not null,
			currency text not null default 'IDR',
			status text not null,
			visibility text not null default 'public',
			is_spam integer not null default 0,
			is_test integer not null default 0,
			moderation_note text not null default '',
			provider text not null,
			provider_order_id text not null default '',
			provider_status text not null default '',
			provider_payment_url text not null default '',
			provider_payment_method text not null default '',
			provider_payment_number text not null default '',
			provider_fee integer not null default 0,
			provider_total_payment integer not null default 0,
			provider_expired_at text,
			provider_completed_at text,
			settlement_source text not null default '',
			manual_reference text not null default '',
			admin_notified_at text,
			paid_at text,
			created_at text not null,
			updated_at text not null
		);

		create table if not exists project_updates (
			id integer primary key autoincrement,
			project_id integer not null references projects(id),
			title text not null,
			body text not null,
			published_at text not null,
			created_at text not null,
			updated_at text not null
		);

		create table if not exists project_expenses (
			id integer primary key autoincrement,
			project_id integer not null references projects(id),
			category text not null,
			description text not null,
			vendor text not null default '',
			reference text not null default '',
			amount integer not null,
			currency text not null default 'IDR',
			visibility text not null default 'public',
			is_voided integer not null default 0,
			voided_at text,
			moderation_note text not null default '',
			incurred_at text not null,
			created_at text not null,
			updated_at text not null
		);

		create table if not exists admin_login_tokens (
			id integer primary key autoincrement,
			email text not null,
			token_hash text not null unique,
			expires_at text not null,
			used_at text,
			created_at text not null
		);

		create table if not exists admin_sessions (
			id integer primary key autoincrement,
			token_hash text not null unique,
			expires_at text not null,
			created_at text not null
		);

	`)
	if err != nil {
		return err
	}

	alterStatements := []string{
		`alter table projects add column repo_url text not null default ''`,
		`alter table projects add column demo_url text not null default ''`,
		`alter table projects add column deadline_date text`,
		`alter table donations add column visibility text not null default 'public'`,
		`alter table donations add column is_spam integer not null default 0`,
		`alter table donations add column is_test integer not null default 0`,
		`alter table donations add column moderation_note text not null default ''`,
		`alter table donations add column provider_status text not null default ''`,
		`alter table donations add column provider_payment_method text not null default ''`,
		`alter table donations add column provider_fee integer not null default 0`,
		`alter table donations add column provider_total_payment integer not null default 0`,
		`alter table donations add column provider_expired_at text`,
		`alter table donations add column provider_completed_at text`,
		`alter table donations add column settlement_source text not null default ''`,
		`alter table donations add column manual_reference text not null default ''`,
		`alter table donations add column admin_notified_at text`,
	}
	for _, statement := range alterStatements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `
		update donations
		set settlement_source = case
			when provider = 'pakasir' then 'pakasir'
			when provider = 'mock' then 'mock'
			else provider
		end
		where status = 'paid' and settlement_source = ''
	`)
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) seed(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `select count(*) from projects`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := "2026-07-04 04:00:00"
	projects := []app.Project{
		{
			Title:       "FolioCMS",
			Slug:        "foliocms",
			Summary:     "Small CMS for fast personal publishing and client sites.",
			Description: "FolioCMS is a compact publishing system for personal websites, client pages, and documentation-heavy projects.\n\nCurrent work is focused on theme APIs, content editing flow, and release paths that stay understandable for self-hosted installs.\n\nSupport helps cover maintenance time, backup guidance, and hardening before wider public use.",
			Status:      "building",
			Goal:        5000000,
			Accent:      "green",
		},
		{
			Title:       "HifzLink",
			Slug:        "hifzlink",
			Summary:     "Tools for memorization progress, review cycles, and teacher notes.",
			Description: "HifzLink helps students, parents, and teachers track memorization progress without spreadsheet drift.\n\nCurrent work covers review scheduling, teacher notes, and cleaner reporting so memorization history stays readable across classes and review cycles.\n\nSupport is used for feature iteration, operational upkeep, and steady improvements to the core learning workflow.",
			Status:      "private beta",
			Goal:        3000000,
			Accent:      "gold",
		},
		{
			Title:       "Plink",
			Slug:        "plink",
			Summary:     "Minimal link shortener with clean analytics and self-hosted deploys.",
			Description: "Plink is a small link shortener for people who want readable links, simple analytics, and a deploy they can understand.\n\nCurrent work is centered on import and export, abuse controls, and operational basics for long-running self-hosted installs.\n\nSupport helps fund maintenance, documentation, and production-ready cleanup around the core short-link flow.",
			Status:      "maintenance",
			Goal:        1500000,
			Accent:      "stone",
		},
	}

	projectIDs := map[string]int64{}
	for _, project := range projects {
		result, err := tx.ExecContext(ctx, `
			insert into projects (title, slug, summary, description, status, goal_amount, accent, is_active, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		`, project.Title, project.Slug, project.Summary, project.Description, project.Status, project.Goal, project.Accent, now, now)
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		projectIDs[project.Slug] = id
	}

	donations := []struct {
		slug           string
		name           string
		email          string
		message        string
		amount         int
		status         string
		visibility     string
		isSpam         int
		provider       string
		providerOrder  string
		providerStatus string
		paidAt         string
		createdAt      string
	}{
		{"foliocms", "", "", "For the theme system and docs work.", 100000, "paid", "public", 0, "mock", "", "completed", "2026-07-04 03:48:00", "2026-07-04 03:48:00"},
		{"hifzlink", "Rafi", "rafi@example.com", "Keep going on teacher dashboard.", 75000, "paid", "public", 0, "mock", "", "completed", "2026-07-03 09:00:00", "2026-07-03 09:00:00"},
		{"foliocms", "Naufal", "naufal@example.com", "Self-hosted CMS matters.", 245000, "paid", "hidden", 0, "mock", "", "completed", "2026-07-02 16:20:00", "2026-07-02 16:20:00"},
		{"plink", "Mira", "mira@example.com", "For export/import support.", 125000, "paid", "public", 1, "mock", "", "completed", "2026-06-30 11:10:00", "2026-06-30 11:10:00"},
		{"foliocms", "Alya", "alya@example.com", "Testing QRIS payment flow.", 50000, "pending_payment", "public", 0, "pakasir", "DON-SEED-001", "pending", "", "2026-07-04 07:15:00"},
		{"hifzlink", "Bima", "bima@example.com", "Expired payment sample.", 25000, "expired", "public", 0, "pakasir", "DON-SEED-002", "expired", "", "2026-07-03 12:40:00"},
		{"plink", "Sinta", "sinta@example.com", "Cancelled by donor.", 30000, "cancelled", "hidden", 0, "pakasir", "DON-SEED-003", "cancelled", "", "2026-07-01 10:05:00"},
	}
	for _, donation := range donations {
		_, err := tx.ExecContext(ctx, `
			insert into donations (
				project_id, donor_name, donor_email, message, amount, currency,
				status, visibility, is_spam, moderation_note,
				provider, provider_order_id, provider_status,
				paid_at, created_at, updated_at
			)
			values (?, ?, ?, ?, ?, 'IDR', ?, ?, ?, '', ?, ?, ?, ?, ?, ?)
		`, projectIDs[donation.slug], donation.name, donation.email, donation.message, donation.amount, donation.status, donation.visibility, donation.isSpam, donation.provider, donation.providerOrder, donation.providerStatus, nullIfEmpty(donation.paidAt), donation.createdAt, donation.createdAt)
		if err != nil {
			return err
		}
	}

	updates := []struct {
		slug        string
		title       string
		body        string
		publishedAt string
	}{
		{"foliocms", "Theme API draft complete", "Template inheritance and asset loading are now sketched out for first implementation.", "2026-07-02 07:00:00"},
		{"hifzlink", "Teacher notes pass", "Private beta now has basic teacher notes wired into student review pages.", "2026-06-29 08:15:00"},
	}
	for _, update := range updates {
		_, err := tx.ExecContext(ctx, `
			insert into project_updates (project_id, title, body, published_at, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?)
		`, projectIDs[update.slug], update.title, update.body, update.publishedAt, update.publishedAt, update.publishedAt)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func relativeTime(value string) string {
	t, err := time.Parse("2006-01-02 15:04:05", value)
	if err != nil {
		return "baru saja"
	}

	diff := time.Since(t)
	if diff < time.Minute {
		return "baru saja"
	}
	if diff < time.Hour {
		return fmt.Sprintf("%d menit lalu", int(diff.Minutes()))
	}
	if diff < 48*time.Hour {
		return fmt.Sprintf("%d jam lalu", int(diff.Hours()))
	}
	if diff < 14*24*time.Hour {
		return fmt.Sprintf("%d hari lalu", int(diff.Hours()/24))
	}
	return t.In(jakartaLocation()).Format("2 Jan 2006")
}

func displayJakartaTime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	t, err := time.Parse("2006-01-02 15:04:05", value)
	if err != nil {
		return value
	}
	return t.In(jakartaLocation()).Format("2 Jan 2006 15:04 WIB")
}

func displayJakartaDate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	t, err := time.Parse("2006-01-02 15:04:05", value)
	if err != nil {
		return value
	}
	return t.In(jakartaLocation()).Format("2 Jan 2006")
}

func jakartaLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err == nil {
		return loc
	}
	return time.FixedZone("WIB", 7*60*60)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func hashLoginToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
