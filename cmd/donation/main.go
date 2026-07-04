package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/srmdn/donation/internal/app"
	"github.com/srmdn/donation/internal/store"
)

//go:embed web/templates/*.html web/static/*
var assets embed.FS

const minDonationAmount = 25000

func main() {
	addr := env("ADDR", "127.0.0.1:8094")
	dbPath := env("DB_PATH", "data/donation.db")
	adminPassword := env("ADMIN_PASSWORD", "admin")
	adminSessionSecret := env("ADMIN_SESSION_SECRET", adminPassword)
	staticFS := mustSubFS(assets, "web/static")
	db, err := store.Open(dbPath)
	if err != nil {
		slog.Error("open store", "error", err, "path", dbPath)
		os.Exit(1)
	}
	defer db.Close()

	tmpl := template.Must(template.New("index.html").Funcs(template.FuncMap{
		"rupiah":         rupiah,
		"percent":        percent,
		"eventHasAmount": eventHasAmount,
	}).ParseFS(assets, "web/templates/*.html"))

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		timelineLimit := timelineLimitFromRequest(r, 6)
		data, err := db.PageDataWithTimelineLimit(r.Context(), timelineLimit)
		if err != nil {
			slog.Error("load page data", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
			slog.Error("render index", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /projects", func(w http.ResponseWriter, r *http.Request) {
		page := pageFromRequest(r)
		limit := 12
		offset := (page - 1) * limit

		projects, hasNext, err := db.ListProjectsPage(r.Context(), limit, offset)
		if err != nil {
			slog.Error("list projects page", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}
		totalProjects, err := db.CountActiveProjects(r.Context())
		if err != nil {
			slog.Error("count active projects", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "projects.html", app.ProjectsIndexPageData{
			Builder: app.Builder{
				Name:   "Said Ramadhan",
				Handle: "srmdn",
				Bio:    "I build small, durable tools for publishing, learning, and self-hosted workflows.",
			},
			Projects:      projects,
			Page:          page,
			HasPrev:       page > 1,
			HasNext:       hasNext,
			PrevPage:      page - 1,
			NextPage:      page + 1,
			TotalProjects: totalProjects,
		}); err != nil {
			slog.Error("render projects index", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /projects/{slug}", func(w http.ResponseWriter, r *http.Request) {
		data, err := db.PageDataWithTimelineLimit(r.Context(), 6)
		if err != nil {
			slog.Error("load page data", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		project, err := db.FindProject(r.Context(), r.PathValue("slug"))
		if errors.Is(err, store.ErrNotFound()) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			slog.Error("load project", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		projectTimelineLimit := timelineLimitFromRequest(r, 5)
		timeline, hasMore, err := db.ListTimeline(r.Context(), project.Slug, projectTimelineLimit)
		if err != nil {
			slog.Error("load project timeline", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}
		data.Timeline = timeline
		data.TimelineHasMore = hasMore
		data.TimelineNextLimit = projectTimelineLimit + 5

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "project.html", app.ProjectPageData{
			PageData: data,
			Project:  project,
		}); err != nil {
			slog.Error("render project", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /donations", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		slug := strings.TrimSpace(r.FormValue("project_slug"))
		if slug == "" {
			http.Error(w, "missing project", http.StatusBadRequest)
			return
		}
		amount, err := donationAmountFromRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		id, err := db.CreatePendingDonation(
			r.Context(),
			slug,
			strings.TrimSpace(r.FormValue("name")),
			strings.TrimSpace(r.FormValue("email")),
			strings.TrimSpace(r.FormValue("message")),
			amount,
		)
		if err != nil {
			slog.Error("create donation", "error", err)
			http.Error(w, "donation failed", http.StatusInternalServerError)
			return
		}

		// Mock mode: mark paid immediately so progress/timeline behavior is visible before Pakasir.
		if env("PAYMENT_MODE", "mock") == "mock" {
			if err := db.MarkDonationPaid(r.Context(), id); err != nil {
				slog.Error("mark mock donation paid", "error", err)
				http.Error(w, "donation failed", http.StatusInternalServerError)
				return
			}
		}

		http.Redirect(w, r, "/thanks?id="+strconv.FormatInt(id, 10), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /thanks", func(w http.ResponseWriter, r *http.Request) {
		data, err := db.PageDataWithTimelineLimit(r.Context(), 6)
		if err != nil {
			slog.Error("load page data", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "thanks.html", data); err != nil {
			slog.Error("render thanks", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r, adminSessionSecret) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/projects", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /admin/login", func(w http.ResponseWriter, r *http.Request) {
		if isAdmin(r, adminSessionSecret) {
			http.Redirect(w, r, "/admin/projects", http.StatusSeeOther)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin_login.html", app.AdminLoginPageData{
			Error: strings.TrimSpace(r.URL.Query().Get("error")),
		}); err != nil {
			slog.Error("render admin login", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /admin/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		password := strings.TrimSpace(r.FormValue("password"))
		if subtle.ConstantTimeCompare([]byte(password), []byte(adminPassword)) != 1 {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("Wrong password"), http.StatusSeeOther)
			return
		}

		setAdminCookie(w, adminSessionSecret)
		http.Redirect(w, r, "/admin/projects?notice="+url.QueryEscape("Signed in"), http.StatusSeeOther)
	})
	mux.HandleFunc("POST /admin/logout", func(w http.ResponseWriter, r *http.Request) {
		clearAdminCookie(w)
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /admin/projects", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r, adminSessionSecret) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		projects, err := db.ListAllProjects(r.Context())
		if err != nil {
			slog.Error("list admin projects", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		page := app.AdminProjectsPageData{
			Projects: projects,
			Error:    strings.TrimSpace(r.URL.Query().Get("error")),
			Notice:   strings.TrimSpace(r.URL.Query().Get("notice")),
		}
		for _, project := range projects {
			if project.IsActive {
				page.ActiveCount++
			}
		}

		editID := strings.TrimSpace(r.URL.Query().Get("edit"))
		if editID != "" {
			id, err := strconv.ParseInt(editID, 10, 64)
			if err == nil {
				project, err := db.FindProjectByID(r.Context(), id)
				if err == nil {
					page.Editing = project
					page.HasEditing = true
				}
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin_projects.html", page); err != nil {
			slog.Error("render admin projects", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /admin/donations", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r, adminSessionSecret) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		donations, err := db.ListAdminDonations(r.Context(), 100)
		if err != nil {
			slog.Error("list admin donations", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		page := app.AdminDonationsPageData{
			Donations: donations,
			Error:     strings.TrimSpace(r.URL.Query().Get("error")),
			Notice:    strings.TrimSpace(r.URL.Query().Get("notice")),
		}
		for _, donation := range donations {
			page.TotalCount++
			if donation.Status == "paid" {
				page.PaidCount++
			}
			if donation.Status == "pending_payment" {
				page.PendingCount++
			}
			if donation.Visibility == "public" {
				page.PublicCount++
			}
			if donation.IsSpam {
				page.SpamCount++
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin_donations.html", page); err != nil {
			slog.Error("render admin donations", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /admin/projects", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r, adminSessionSecret) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		project, err := projectFromRequest(r)
		if err != nil {
			http.Redirect(w, r, "/admin/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		if err := db.CreateProject(r.Context(), project); err != nil {
			slog.Error("create project", "error", err)
			http.Redirect(w, r, "/admin/projects?error="+url.QueryEscape("Create failed"), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/projects?notice="+url.QueryEscape("Project created"), http.StatusSeeOther)
	})
	mux.HandleFunc("POST /admin/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r, adminSessionSecret) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid project id", http.StatusBadRequest)
			return
		}

		project, err := projectFromRequest(r)
		if err != nil {
			http.Redirect(w, r, "/admin/projects?edit="+strconv.FormatInt(id, 10)+"&error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		project.ID = id
		if err := db.UpdateProject(r.Context(), project); err != nil {
			slog.Error("update project", "error", err)
			http.Redirect(w, r, "/admin/projects?edit="+strconv.FormatInt(id, 10)+"&error="+url.QueryEscape("Update failed"), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/projects?notice="+url.QueryEscape("Project updated"), http.StatusSeeOther)
	})
	mux.HandleFunc("POST /admin/donations/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r, adminSessionSecret) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid donation id", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}

		action := strings.TrimSpace(r.FormValue("action"))
		if err := db.UpdateDonationModeration(r.Context(), id, action); err != nil {
			slog.Error("update donation moderation", "error", err, "id", id, "action", action)
			http.Redirect(w, r, "/admin/donations?error="+url.QueryEscape("Update failed"), http.StatusSeeOther)
			return
		}

		http.Redirect(w, r, "/admin/donations?notice="+url.QueryEscape("Donation updated"), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("starting donation app", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func mustSubFS(root fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(root, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func env(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start).String())
	})
}

func rupiah(amount int) string {
	if amount == 0 {
		return "Rp 0"
	}
	s := strconv.Itoa(amount)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return "Rp " + strings.Join(parts, ".")
}

func percent(raised, goal int) int {
	if goal <= 0 {
		return 0
	}
	p := raised * 100 / goal
	if p > 100 {
		return 100
	}
	return p
}

func eventHasAmount(amount int) bool {
	return amount > 0
}

func donationAmountFromRequest(r *http.Request) (int, error) {
	custom := strings.TrimSpace(r.FormValue("custom_amount"))
	raw := strings.TrimSpace(r.FormValue("amount"))
	if custom != "" {
		raw = custom
	}

	amount, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("invalid amount")
	}
	if amount < minDonationAmount {
		return 0, errors.New("minimum donation is Rp25.000")
	}
	return amount, nil
}

func timelineLimitFromRequest(r *http.Request, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get("timeline_limit"))
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if value > 60 {
		return 60
	}
	return value
}

func pageFromRequest(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("page"))
	if raw == "" {
		return 1
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 1
	}
	return value
}

func projectFromRequest(r *http.Request) (app.Project, error) {
	if err := r.ParseForm(); err != nil {
		return app.Project{}, errors.New("invalid form")
	}

	goal, err := strconv.Atoi(strings.TrimSpace(r.FormValue("goal")))
	if err != nil || goal <= 0 {
		return app.Project{}, errors.New("goal must be a positive number")
	}

	project := app.Project{
		Title:       strings.TrimSpace(r.FormValue("title")),
		Slug:        strings.TrimSpace(r.FormValue("slug")),
		Summary:     strings.TrimSpace(r.FormValue("summary")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Status:      strings.TrimSpace(r.FormValue("status")),
		Goal:        goal,
		Accent:      strings.TrimSpace(r.FormValue("accent")),
		RepoURL:     strings.TrimSpace(r.FormValue("repo_url")),
		DemoURL:     strings.TrimSpace(r.FormValue("demo_url")),
		IsActive:    r.FormValue("is_active") == "on",
	}

	switch {
	case project.Title == "":
		return app.Project{}, errors.New("title is required")
	case project.Slug == "":
		return app.Project{}, errors.New("slug is required")
	case project.Summary == "":
		return app.Project{}, errors.New("summary is required")
	case project.Description == "":
		return app.Project{}, errors.New("description is required")
	case project.Status == "":
		return app.Project{}, errors.New("status is required")
	case project.Accent == "":
		return app.Project{}, errors.New("accent is required")
	}

	return project, nil
}

func isAdmin(r *http.Request, secret string) bool {
	cookie, err := r.Cookie("admin_session")
	if err != nil {
		return false
	}
	expected := adminCookieValue(secret)
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(expected)) == 1
}

func setAdminCookie(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    adminCookieValue(secret),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 30,
	})
}

func clearAdminCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func adminCookieValue(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("admin-session"))
	return hex.EncodeToString(mac.Sum(nil))
}
