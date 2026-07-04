package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	builder := app.Builder{
		Name:   "Said Ramadhan",
		Handle: "srmdn",
		Bio:    "I build small, durable tools for publishing, learning, and self-hosted workflows.",
	}

	projects, err := s.ListProjects(ctx)
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
	if err := s.db.QueryRowContext(ctx, `select count(*) from donations where status = 'paid'`).Scan(&supporters); err != nil {
		return app.PageData{}, err
	}

	return app.PageData{
		Builder:           builder,
		TotalRaised:       total,
		SupporterCount:    supporters,
		ActiveProjectNum:  len(projects),
		Projects:          projects,
		Timeline:          timeline,
		TimelineHasMore:   hasMore,
		TimelineNextLimit: limit + 6,
	}, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]app.Project, error) {
	return s.listProjects(ctx, true)
}

func (s *Store) ListAllProjects(ctx context.Context) ([]app.Project, error) {
	return s.listProjects(ctx, false)
}

func (s *Store) listProjects(ctx context.Context, activeOnly bool) ([]app.Project, error) {
	where := ""
	if activeOnly {
		where = "where p.is_active = 1"
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
			p.is_active,
			coalesce(sum(case when d.status = 'paid' then d.amount else 0 end), 0) as raised,
			coalesce(max(case when u.published_at is not null then u.published_at end), p.updated_at) as last_updated
		from projects p
		left join donations d on d.project_id = p.id
		left join project_updates u on u.project_id = p.id
		`+where+`
		group by p.id
		order by p.id asc
	`)
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
			&project.IsActive,
			&project.Raised,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		project.LastUpdated = relativeTime(updatedAt)
		projects = append(projects, project)
	}
	return projects, rows.Err()
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
					when donor_name = '' then 'Anonymous supported ' || p.title
					else donor_name || ' supported ' || p.title
				end as title,
				coalesce(nullif(message, ''), 'No public message.') as detail,
				d.amount as amount,
				p.title as project,
				d.paid_at as occurred_at,
				p.slug as slug
			from donations d
			join projects p on p.id = d.project_id
			where d.status = 'paid'
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

func (s *Store) CreatePendingDonation(ctx context.Context, projectSlug, name, email, message string, amount int) (int64, error) {
	if amount <= 0 {
		return 0, errors.New("amount must be positive")
	}

	var projectID int64
	if err := s.db.QueryRowContext(ctx, `select id from projects where slug = ?`, projectSlug).Scan(&projectID); err != nil {
		return 0, err
	}

	result, err := s.db.ExecContext(ctx, `
		insert into donations (project_id, donor_name, donor_email, message, amount, currency, status, provider, created_at, updated_at)
		values (?, ?, ?, ?, ?, 'IDR', 'pending', 'mock', datetime('now'), datetime('now'))
	`, projectID, name, email, message, amount)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) MarkDonationPaid(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		update donations
		set status = 'paid', paid_at = datetime('now'), updated_at = datetime('now')
		where id = ?
	`, id)
	return err
}

func (s *Store) CreateProject(ctx context.Context, project app.Project) error {
	_, err := s.db.ExecContext(ctx, `
		insert into projects (title, slug, summary, description, status, goal_amount, accent, is_active, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	`, project.Title, project.Slug, project.Summary, project.Description, project.Status, project.Goal, project.Accent, boolToInt(project.IsActive))
	return err
}

func (s *Store) UpdateProject(ctx context.Context, project app.Project) error {
	result, err := s.db.ExecContext(ctx, `
		update projects
		set title = ?, slug = ?, summary = ?, description = ?, status = ?, goal_amount = ?, accent = ?, is_active = ?, updated_at = datetime('now')
		where id = ?
	`, project.Title, project.Slug, project.Summary, project.Description, project.Status, project.Goal, project.Accent, boolToInt(project.IsActive), project.ID)
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
			provider text not null,
			provider_order_id text not null default '',
			provider_payment_url text not null default '',
			provider_payment_number text not null default '',
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
	`)
	return err
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
			Description: "FolioCMS is a compact publishing system for personal websites, client pages, and documentation-heavy projects. Support goes toward theme APIs, content editing, backup docs, and stable self-hosted releases.",
			Status:      "building",
			Goal:        5000000,
			Accent:      "green",
		},
		{
			Title:       "HifzLink",
			Slug:        "hifzlink",
			Summary:     "Tools for memorization progress, review cycles, and teacher notes.",
			Description: "HifzLink helps students, parents, and teachers track memorization progress without spreadsheet drift. Support goes toward review scheduling, teacher notes, and cleaner progress reports.",
			Status:      "private beta",
			Goal:        3000000,
			Accent:      "gold",
		},
		{
			Title:       "Plink",
			Slug:        "plink",
			Summary:     "Minimal link shortener with clean analytics and self-hosted deploys.",
			Description: "Plink is a small link shortener for people who want readable links, simple analytics, and a deploy they can understand. Support goes toward import/export, abuse controls, and docs.",
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
		slug    string
		name    string
		message string
		amount  int
		paidAt  string
	}{
		{"foliocms", "", "For the theme system and docs work.", 100000, "2026-07-04 03:48:00"},
		{"hifzlink", "Rafi", "Keep going on teacher dashboard.", 75000, "2026-07-03 09:00:00"},
		{"foliocms", "Naufal", "Self-hosted CMS matters.", 245000, "2026-07-02 16:20:00"},
		{"plink", "Mira", "For export/import support.", 125000, "2026-06-30 11:10:00"},
	}
	for _, donation := range donations {
		_, err := tx.ExecContext(ctx, `
			insert into donations (project_id, donor_name, message, amount, currency, status, provider, paid_at, created_at, updated_at)
			values (?, ?, ?, ?, 'IDR', 'paid', 'mock', ?, ?, ?)
		`, projectIDs[donation.slug], donation.name, donation.message, donation.amount, donation.paidAt, donation.paidAt, donation.paidAt)
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
		return "recently"
	}

	diff := time.Since(t)
	if diff < time.Minute {
		return "just now"
	}
	if diff < time.Hour {
		return fmt.Sprintf("%d min ago", int(diff.Minutes()))
	}
	if diff < 48*time.Hour {
		return fmt.Sprintf("%d hr ago", int(diff.Hours()))
	}
	if diff < 14*24*time.Hour {
		return fmt.Sprintf("%d days ago", int(diff.Hours()/24))
	}
	return t.Format("2 Jan 2006")
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
