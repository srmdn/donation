package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srmdn/donation/internal/app"
	"github.com/srmdn/donation/internal/csrf"
	"github.com/srmdn/donation/internal/mailer"
	"github.com/srmdn/donation/internal/payment"
	"github.com/srmdn/donation/internal/store"
)

//go:embed web/templates/*.html web/static/*
var assets embed.FS

const minDonationAmount = 25000

type loginRateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	entries map[string]loginRateEntry
}

type loginRateEntry struct {
	count   int
	expires time.Time
}

func main() {
	loadDotenv(".env")

	addr := env("ADDR", "127.0.0.1:8094")
	dbPath := env("DB_PATH", "data/donation.db")
	publicBaseURL := env("PUBLIC_BASE_URL", "")
	adminEmail := strings.TrimSpace(strings.ToLower(env("ADMIN_EMAIL", "")))
	adminSessionSecret := env("ADMIN_SESSION_SECRET", "change-me")
	adminCookieSecure := strings.HasPrefix(strings.ToLower(publicBaseURL), "https://")
	paymentMode := env("PAYMENT_MODE", "mock")
	allowLoggedMagicLink := isLocalPublicBaseURL(publicBaseURL)
	if !allowLoggedMagicLink && invalidAdminSessionSecret(adminSessionSecret) {
		slog.Error("invalid admin session secret for non-local deployment")
		os.Exit(1)
	}
	adminMailer := mailer.New(
		env("SMTP_HOST", ""),
		envInt("SMTP_PORT", 587),
		env("SMTP_USERNAME", ""),
		env("SMTP_PASSWORD", ""),
		env("MAIL_FROM", ""),
	)
	adminLoginLimiter := newLoginRateLimiter(5, 15*time.Minute)
	adminVerifyLimiter := newLoginRateLimiter(10, 15*time.Minute)
	webhookLimiter := newLoginRateLimiter(60, time.Minute)
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
		data.CSRFToken = csrfToken(w, r, adminSessionSecret, adminCookieSecure)

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
		if !requireCSRF(r, adminSessionSecret) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
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
		if paymentMode == "mock" {
			if err := db.MarkDonationPaid(r.Context(), id); err != nil {
				slog.Error("mark mock donation paid", "error", err)
				http.Error(w, "donation failed", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/thanks?id="+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}

		if paymentMode != "pakasir" {
			http.Error(w, "unsupported payment mode", http.StatusInternalServerError)
			return
		}

		donation, err := db.FindDonationByID(r.Context(), id)
		if err != nil {
			slog.Error("load donation after create", "error", err, "id", id)
			http.Error(w, "donation failed", http.StatusInternalServerError)
			return
		}

		pakasir := pakasirClientFromEnv()
		redirectURL := ""
		if publicBaseURL != "" {
			redirectURL = strings.TrimRight(publicBaseURL, "/") + "/thanks?id=" + strconv.FormatInt(id, 10)
		}
		orderID := pakasirOrderID(id)
		result, err := pakasir.CreateQRISTransaction(r.Context(), orderID, donation.Amount, redirectURL)
		if err != nil {
			slog.Error("create pakasir transaction", "error", err, "id", id)
			http.Error(w, "payment setup failed", http.StatusInternalServerError)
			return
		}

		donation.Provider = "pakasir"
		donation.ProviderOrderID = result.OrderID
		donation.ProviderPaymentURL = result.PaymentURL
		donation.ProviderPaymentMethod = result.PaymentMethod
		donation.ProviderPaymentNumber = result.PaymentNumber
		donation.ProviderFee = result.Fee
		donation.ProviderTotalPayment = result.TotalPayment
		donation.ProviderExpiredAt = result.ExpiredAt
		donation.ProviderStatus = result.ProviderStatus
		if err := db.UpdateDonationPaymentDraft(r.Context(), donation); err != nil {
			slog.Error("store pakasir transaction", "error", err, "id", id)
			http.Error(w, "payment setup failed", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/pay/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /pay/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid donation id", http.StatusBadRequest)
			return
		}

		donation, err := db.FindDonationByID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound()) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			slog.Error("load payment page donation", "error", err, "id", id)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}
		if donation.Status == "paid" {
			http.Redirect(w, r, "/thanks?id="+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "pay.html", app.PayPageData{
			Builder:  defaultBuilder(),
			Donation: donation,
		}); err != nil {
			slog.Error("render pay page", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /thanks", func(w http.ResponseWriter, r *http.Request) {
		page := app.ThanksPageData{
			Builder: defaultBuilder(),
		}

		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id != "" {
			if donationID, err := strconv.ParseInt(id, 10, 64); err == nil {
				donation, err := db.FindDonationByID(r.Context(), donationID)
				if err == nil {
					page.HasID = true
					if paymentMode == "pakasir" && donation.Provider == "pakasir" && donation.Status != "paid" && donation.ProviderOrderID != "" {
						if err := refreshDonationStatus(r.Context(), db, pakasirClientFromEnv(), donation); err != nil {
							slog.Warn("refresh donation status", "error", err, "id", donation.ID)
						} else if updated, err := db.FindDonationByID(r.Context(), donationID); err == nil {
							donation = updated
						}
					}
					page.Donation = donation
				}
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "thanks.html", page); err != nil {
			slog.Error("render thanks", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /api/webhooks/pakasir", func(w http.ResponseWriter, r *http.Request) {
		if paymentMode != "pakasir" {
			http.Error(w, "payment mode disabled", http.StatusNotFound)
			return
		}
		if !webhookLimiter.Allow(clientIP(r), time.Now()) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)

		var payload struct {
			Amount        int    `json:"amount"`
			OrderID       string `json:"order_id"`
			Project       string `json:"project"`
			Status        string `json:"status"`
			PaymentMethod string `json:"payment_method"`
			CompletedAt   string `json:"completed_at"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		payload.OrderID = strings.TrimSpace(payload.OrderID)
		payload.Project = strings.TrimSpace(payload.Project)
		if payload.OrderID == "" || payload.Amount <= 0 || payload.Project == "" {
			http.Error(w, "missing required webhook fields", http.StatusBadRequest)
			return
		}
		if payload.Project != env("PAKASIR_MERCHANT_SLUG", "") {
			http.Error(w, "project mismatch", http.StatusBadRequest)
			return
		}

		donation, err := db.FindDonationByOrderID(r.Context(), payload.OrderID)
		if errors.Is(err, store.ErrNotFound()) {
			http.Error(w, "donation not found", http.StatusNotFound)
			return
		}
		if err != nil {
			slog.Error("find donation by order id", "error", err, "order_id", payload.OrderID)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if donation.Amount != payload.Amount {
			http.Error(w, "amount mismatch", http.StatusBadRequest)
			return
		}

		if err := refreshDonationStatus(r.Context(), db, pakasirClientFromEnv(), donation); err != nil {
			slog.Error("refresh donation status from webhook", "error", err, "donation_id", donation.ID, "order_id", payload.OrderID)
			http.Error(w, "verification failed", http.StatusBadGateway)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r.Context(), r, db) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/projects", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /admin/login", func(w http.ResponseWriter, r *http.Request) {
		if isAdmin(r.Context(), r, db) {
			http.Redirect(w, r, "/admin/projects", http.StatusSeeOther)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin_login.html", app.AdminLoginPageData{
			Error:     strings.TrimSpace(r.URL.Query().Get("error")),
			Notice:    strings.TrimSpace(r.URL.Query().Get("notice")),
			Email:     strings.TrimSpace(r.URL.Query().Get("email")),
			CSRFToken: csrfToken(w, r, adminSessionSecret, adminCookieSecure),
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
		if !requireCSRF(r, adminSessionSecret) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}

		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
		if email == "" {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("Email is required"), http.StatusSeeOther)
			return
		}
		if adminEmail == "" {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("Admin email is not configured"), http.StatusSeeOther)
			return
		}
		if !adminLoginLimiter.Allow(adminRateLimitKey(r, email), time.Now()) {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("Too many sign-in attempts. Wait a few minutes and try again.")+"&email="+url.QueryEscape(email), http.StatusSeeOther)
			return
		}

		notice := "If that email can access admin, a sign-in link is ready."
		if subtle.ConstantTimeCompare([]byte(email), []byte(adminEmail)) == 1 {
			if !adminMailer.Configured() && !allowLoggedMagicLink {
				slog.Error("admin mail delivery is not configured for this environment")
				http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("Admin mail delivery is not configured"), http.StatusSeeOther)
				return
			}
			token, err := generateLoginToken()
			if err != nil {
				slog.Error("generate admin login token", "error", err)
				http.Error(w, "login failed", http.StatusInternalServerError)
				return
			}
			expiresAt := time.Now().Add(15 * time.Minute)
			if err := db.CreateAdminLoginToken(r.Context(), email, token, expiresAt); err != nil {
				slog.Error("store admin login token", "error", err)
				http.Error(w, "login failed", http.StatusInternalServerError)
				return
			}
			if err := adminMailer.SendMagicLink(email, adminMagicLoginURL(publicBaseURL, token)); err != nil {
				slog.Error("send admin magic link", "error", err)
				http.Error(w, "login failed", http.StatusInternalServerError)
				return
			}
		}

		http.Redirect(w, r, "/admin/login?notice="+url.QueryEscape(notice)+"&email="+url.QueryEscape(email), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /admin/login/verify", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.URL.Query().Get("token"))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin_login_verify.html", app.AdminLoginVerifyPageData{
			Error:     strings.TrimSpace(r.URL.Query().Get("error")),
			Token:     token,
			CSRFToken: csrfToken(w, r, adminSessionSecret, adminCookieSecure),
		}); err != nil {
			slog.Error("render admin login verify", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /admin/login/verify", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !requireCSRF(r, adminSessionSecret) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}

		token := strings.TrimSpace(r.FormValue("token"))
		if token == "" {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("That sign-in link is invalid or expired"), http.StatusSeeOther)
			return
		}
		if !adminVerifyLimiter.Allow(adminRateLimitKey(r, token), time.Now()) {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("Too many sign-in attempts. Wait a few minutes and try again."), http.StatusSeeOther)
			return
		}

		email, err := db.ConsumeAdminLoginToken(r.Context(), token, time.Now())
		if errors.Is(err, store.ErrInvalidAdminLoginTokenError()) {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("That sign-in link is invalid or expired"), http.StatusSeeOther)
			return
		}
		if err != nil {
			slog.Error("consume admin login token", "error", err)
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}
		if adminEmail == "" || subtle.ConstantTimeCompare([]byte(strings.ToLower(email)), []byte(adminEmail)) != 1 {
			http.Redirect(w, r, "/admin/login?error="+url.QueryEscape("That sign-in link is invalid or expired"), http.StatusSeeOther)
			return
		}

		sessionToken, err := generateLoginToken()
		if err != nil {
			slog.Error("generate admin session token", "error", err)
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}
		sessionExpiresAt := time.Now().Add(30 * 24 * time.Hour)
		if err := db.CreateAdminSession(r.Context(), sessionToken, sessionExpiresAt); err != nil {
			slog.Error("store admin session", "error", err)
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}

		setAdminCookie(w, sessionToken, adminCookieSecure, sessionExpiresAt)
		http.Redirect(w, r, "/admin/projects?notice="+url.QueryEscape("Signed in"), http.StatusSeeOther)
	})
	mux.HandleFunc("POST /admin/logout", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !requireCSRF(r, adminSessionSecret) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		if token := adminSessionToken(r); token != "" {
			if err := db.DeleteAdminSession(r.Context(), token); err != nil {
				slog.Error("delete admin session", "error", err)
				http.Error(w, "logout failed", http.StatusInternalServerError)
				return
			}
		}
		clearAdminCookie(w, adminCookieSecure)
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /admin/projects", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r.Context(), r, db) {
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
			Projects:  projects,
			Error:     strings.TrimSpace(r.URL.Query().Get("error")),
			Notice:    strings.TrimSpace(r.URL.Query().Get("notice")),
			CSRFToken: csrfToken(w, r, adminSessionSecret, adminCookieSecure),
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
		if !isAdmin(r.Context(), r, db) {
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
			CSRFToken: csrfToken(w, r, adminSessionSecret, adminCookieSecure),
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
		if !isAdmin(r.Context(), r, db) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !requireCSRF(r, adminSessionSecret) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
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
		if !isAdmin(r.Context(), r, db) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !requireCSRF(r, adminSessionSecret) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
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
		if !isAdmin(r.Context(), r, db) {
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
		if !requireCSRF(r, adminSessionSecret) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}

		action := strings.TrimSpace(r.FormValue("action"))
		if action == "refresh_status" {
			donation, err := db.FindDonationByID(r.Context(), id)
			if err != nil {
				slog.Error("find donation for refresh", "error", err, "id", id)
				http.Redirect(w, r, "/admin/donations?error="+url.QueryEscape("Donation not found"), http.StatusSeeOther)
				return
			}
			if err := refreshDonationStatus(r.Context(), db, pakasirClientFromEnv(), donation); err != nil {
				slog.Error("refresh donation status", "error", err, "id", id)
				http.Redirect(w, r, "/admin/donations?error="+url.QueryEscape("Refresh failed"), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/admin/donations?notice="+url.QueryEscape("Donation status refreshed"), http.StatusSeeOther)
			return
		}

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

func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx == -1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[0] == val[len(val)-1] {
			val = val[1 : len(val)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func defaultBuilder() app.Builder {
	return app.Builder{
		Name:   "Said Ramadhan",
		Handle: "srmdn",
		Bio:    "I build small, durable tools for publishing, learning, and self-hosted workflows.",
	}
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

func pakasirOrderID(id int64) string {
	return "DON-" + strconv.FormatInt(id, 10)
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

func isAdmin(ctx context.Context, r *http.Request, db *store.Store) bool {
	token := adminSessionToken(r)
	if token == "" {
		return false
	}
	ok, err := db.HasActiveAdminSession(ctx, token, time.Now())
	if err != nil {
		slog.Error("check admin session", "error", err)
		return false
	}
	return ok
}

func adminSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("admin_session")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

func setAdminCookie(w http.ResponseWriter, token string, secure bool, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   maxAge,
	})
}

func clearAdminCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func csrfToken(w http.ResponseWriter, r *http.Request, secret string, secure bool) string {
	if cookie, err := r.Cookie(csrf.CookieName); err == nil && csrf.Validate(cookie.Value, cookie.Value, secret) {
		return cookie.Value
	}
	token := csrf.NewToken(secret)
	http.SetCookie(w, csrf.Cookie(token, secure))
	return token
}

func requireCSRF(r *http.Request, secret string) bool {
	cookie, err := r.Cookie(csrf.CookieName)
	if err != nil {
		return false
	}
	return csrf.Validate(cookie.Value, r.FormValue(csrf.FormField), secret)
}

func generateLoginToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newLoginRateLimiter(limit int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{
		window:  window,
		limit:   limit,
		entries: make(map[string]loginRateEntry),
	}
}

func (l *loginRateLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	for existingKey, entry := range l.entries {
		if now.After(entry.expires) {
			delete(l.entries, existingKey)
		}
	}

	entry := l.entries[key]
	if now.After(entry.expires) {
		entry = loginRateEntry{expires: now.Add(l.window)}
	}
	if entry.count >= l.limit {
		l.entries[key] = entry
		return false
	}
	entry.count++
	if entry.expires.IsZero() {
		entry.expires = now.Add(l.window)
	}
	l.entries[key] = entry
	return true
}

func adminMagicLoginURL(baseURL, token string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8094"
	}
	return base + "/admin/login/verify#token=" + url.QueryEscape(token)
}

func adminRateLimitKey(r *http.Request, value string) string {
	return clientIP(r) + "|" + strings.ToLower(strings.TrimSpace(value))
}

func isLocalPublicBaseURL(baseURL string) bool {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func invalidAdminSessionSecret(secret string) bool {
	secret = strings.TrimSpace(secret)
	return secret == "" || secret == "change-me"
}

func clientIP(r *http.Request) string {
	for _, header := range []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"} {
		raw := strings.TrimSpace(r.Header.Get(header))
		if raw == "" {
			continue
		}
		if header == "X-Forwarded-For" {
			raw = strings.TrimSpace(strings.Split(raw, ",")[0])
		}
		if raw != "" {
			return raw
		}
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func pakasirClientFromEnv() payment.PakasirClient {
	return payment.PakasirClient{
		BaseURL:      env("PAKASIR_BASE_URL", "https://app.pakasir.com"),
		APIKey:       env("PAKASIR_API_KEY", ""),
		MerchantSlug: env("PAKASIR_MERCHANT_SLUG", ""),
	}
}

func refreshDonationStatus(ctx context.Context, db *store.Store, client payment.PakasirClient, donation app.Donation) error {
	if donation.ProviderOrderID == "" {
		return nil
	}
	if !client.Enabled() {
		return errors.New("pakasir client is not configured")
	}

	status, err := client.TransactionDetail(ctx, donation.ProviderOrderID, donation.Amount)
	if err != nil {
		return err
	}
	if status.OrderID != donation.ProviderOrderID {
		return errors.New("pakasir order id mismatch")
	}
	if status.Amount != donation.Amount {
		return errors.New("pakasir amount mismatch")
	}
	if status.Project != client.MerchantSlug {
		return errors.New("pakasir project mismatch")
	}

	switch status.Status {
	case "completed":
		return db.UpdateDonationProviderStatus(ctx, donation.ID, "paid", "completed", status.PaymentMethod, status.CompletedAt)
	case "pending":
		return db.UpdateDonationProviderStatus(ctx, donation.ID, "pending_payment", "pending", status.PaymentMethod, "")
	case "cancelled":
		return db.UpdateDonationProviderStatus(ctx, donation.ID, "cancelled", "cancelled", status.PaymentMethod, "")
	case "expired":
		return db.UpdateDonationProviderStatus(ctx, donation.ID, "expired", "expired", status.PaymentMethod, "")
	default:
		return nil
	}
}
