package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
const defaultLocalAddr = "127.0.0.1:8080"
const defaultLocalBaseURL = "http://" + defaultLocalAddr

var staticAssetVersion = map[string]string{}

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

func mustStaticAssetDigests(staticFS fs.FS) map[string]string {
	entries, err := fs.ReadDir(staticFS, ".")
	if err != nil {
		panic(err)
	}
	digests := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := fs.ReadFile(staticFS, entry.Name())
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(content)
		digests[entry.Name()] = hex.EncodeToString(sum[:])[:12]
	}
	staticAssetVersion = digests
	return digests
}

func staticAssetPathFunc(digests map[string]string) func(string) string {
	return func(name string) string {
		if digest, ok := digests[name]; ok {
			return "/static/" + name + "?v=" + digest
		}
		return "/static/" + name
	}
}

func staticAssetPath(name string) string {
	if digest, ok := staticAssetVersion[name]; ok {
		return "/static/" + name + "?v=" + digest
	}
	return "/static/" + name
}

func main() {
	loadDotenv(".env")

	addr := env("ADDR", defaultLocalAddr)
	dbPath := env("DB_PATH", "data/donation.db")
	publicBaseURL := env("PUBLIC_BASE_URL", "")
	appEnv := env("APP_ENV", "development")
	adminEmail := strings.TrimSpace(strings.ToLower(env("ADMIN_EMAIL", "")))
	adminSessionSecret := env("ADMIN_SESSION_SECRET", "change-me")
	adminCookieSecure := strings.HasPrefix(strings.ToLower(publicBaseURL), "https://")
	paymentMode := env("PAYMENT_MODE", "mock")
	allowLoggedMagicLink := isDevelopmentEnv(appEnv) && isLocalPublicBaseURL(publicBaseURL)
	adminMailer := mailer.New(
		env("SMTP_HOST", ""),
		envInt("SMTP_PORT", 587),
		env("SMTP_USERNAME", ""),
		env("SMTP_PASSWORD", ""),
		env("MAIL_FROM", ""),
	)
	if isProductionEnv(appEnv) {
		if invalidAdminSessionSecret(adminSessionSecret) {
			slog.Error("invalid admin session secret for production")
			os.Exit(1)
		}
		if paymentMode == "mock" {
			slog.Error("mock payment mode is not allowed in production")
			os.Exit(1)
		}
		if !adminMailer.Configured() {
			slog.Error("smtp must be configured in production")
			os.Exit(1)
		}
	}
	adminLoginLimiter := newLoginRateLimiter(5, 15*time.Minute)
	adminVerifyLimiter := newLoginRateLimiter(10, 15*time.Minute)
	webhookLimiter := newLoginRateLimiter(60, time.Minute)
	staticFS := mustSubFS(assets, "web/static")
	assetDigests := mustStaticAssetDigests(staticFS)
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
		"assetPath":      staticAssetPathFunc(assetDigests),
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
		data.Meta = homeMeta(publicBaseURL, r, data)

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
			Builder:       app.DefaultBuilder(),
			Projects:      projects,
			Page:          page,
			HasPrev:       page > 1,
			HasNext:       hasNext,
			PrevPage:      page - 1,
			NextPage:      page + 1,
			TotalProjects: totalProjects,
			Meta:          projectsMeta(publicBaseURL, r, totalProjects),
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
		data.Meta = projectMeta(publicBaseURL, r, data.Builder, project)

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
		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
		if email == "" {
			http.Error(w, "email is required", http.StatusBadRequest)
			return
		}
		project, err := db.FindProject(r.Context(), slug)
		if errors.Is(err, store.ErrNotFound()) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		if err != nil {
			slog.Error("load donation project", "error", err, "slug", slug)
			http.Error(w, "donation failed", http.StatusInternalServerError)
			return
		}
		if project.DeadlineEnded {
			http.Error(w, "periode dukungan untuk proyek ini sudah berakhir", http.StatusBadRequest)
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
			email,
			strings.TrimSpace(r.FormValue("message")),
			amount,
		)
		if err != nil {
			slog.Error("create donation", "error", err)
			http.Error(w, "donation failed", http.StatusInternalServerError)
			return
		}

		// Mock mode keeps the donation pending until explicitly confirmed from the payment page.
		if paymentMode == "mock" {
			http.Redirect(w, r, "/pay/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
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
			Builder:   app.DefaultBuilder(),
			Donation:  donation,
			CSRFToken: csrfToken(w, r, adminSessionSecret, adminCookieSecure),
		}); err != nil {
			slog.Error("render pay page", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /pay/{id}/mock-confirm", func(w http.ResponseWriter, r *http.Request) {
		if paymentMode != "mock" {
			http.NotFound(w, r)
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
			http.Error(w, "invalid donation id", http.StatusBadRequest)
			return
		}
		donation, err := db.FindDonationByID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound()) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			slog.Error("load mock confirm donation", "error", err, "id", id)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}
		if donation.Status != "paid" {
			if err := db.MarkDonationPaid(r.Context(), id); err != nil {
				slog.Error("mark mock donation paid", "error", err, "id", id)
				http.Error(w, "payment confirm failed", http.StatusInternalServerError)
				return
			}
			if updated, err := db.FindDonationByID(r.Context(), id); err == nil {
				if err := notifyAdminDonationPaid(r.Context(), db, adminMailer, adminEmail, publicBaseURL, updated); err != nil {
					slog.Error("notify admin donation paid", "error", err, "id", id)
				}
			}
		}
		http.Redirect(w, r, "/thanks?id="+strconv.FormatInt(id, 10), http.StatusSeeOther)
	})
	mux.HandleFunc("GET /thanks", func(w http.ResponseWriter, r *http.Request) {
		page := app.ThanksPageData{
			Builder: app.DefaultBuilder(),
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
							if err := notifyAdminDonationPaid(r.Context(), db, adminMailer, adminEmail, publicBaseURL, donation); err != nil {
								slog.Error("notify admin donation paid", "error", err, "id", donation.ID)
							}
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
		if updated, err := db.FindDonationByID(r.Context(), donation.ID); err == nil {
			if err := notifyAdminDonationPaid(r.Context(), db, adminMailer, adminEmail, publicBaseURL, updated); err != nil {
				slog.Error("notify admin donation paid", "error", err, "id", updated.ID)
			}
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
	mux.HandleFunc("GET /admin/updates", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r.Context(), r, db) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		projects, err := db.ListAllProjects(r.Context())
		if err != nil {
			slog.Error("list projects for admin updates", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}
		updates, err := db.ListAdminProjectUpdates(r.Context(), 30)
		if err != nil {
			slog.Error("list admin project updates", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		page := app.AdminUpdatesPageData{
			Projects:    projects,
			Updates:     updates,
			Error:       strings.TrimSpace(r.URL.Query().Get("error")),
			Notice:      strings.TrimSpace(r.URL.Query().Get("notice")),
			UpdateTitle: strings.TrimSpace(r.URL.Query().Get("update_title")),
			UpdateBody:  strings.TrimSpace(r.URL.Query().Get("update_body")),
			CSRFToken:   csrfToken(w, r, adminSessionSecret, adminCookieSecure),
		}
		page.UpdateProjectID, _ = strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("update_project_id")), 10, 64)
		page.UpdateEditingID, _ = strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("update_edit")), 10, 64)
		if page.UpdateEditingID > 0 && page.UpdateTitle == "" && page.UpdateBody == "" && page.UpdateProjectID == 0 {
			update, err := db.FindProjectUpdateByID(r.Context(), page.UpdateEditingID)
			if err == nil {
				page.UpdateProjectID = update.ProjectID
				page.UpdateTitle = update.Title
				page.UpdateBody = update.Body
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin_updates.html", page); err != nil {
			slog.Error("render admin updates", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /admin/donations", func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r.Context(), r, db) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		filters := adminDonationFilters(r)
		filterStatus := filters.Get("status")
		filterVisibility := filters.Get("visibility")
		filterSpam := filters.Get("spam")
		filterTest := filters.Get("test")
		filterProjectSlug := filters.Get("project")
		searchQuery := filters.Get("q")

		donations, err := db.ListAdminDonations(r.Context(), 100, filterStatus, filterVisibility, filterSpam, filterTest, filterProjectSlug, searchQuery)
		if err != nil {
			slog.Error("list admin donations", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}
		projects, err := db.ListAllProjects(r.Context())
		if err != nil {
			slog.Error("list projects for admin donations", "error", err)
			http.Error(w, "data load failed", http.StatusInternalServerError)
			return
		}

		page := app.AdminDonationsPageData{
			Donations:         donations,
			Projects:          projects,
			Error:             popAdminDonationFlash(w, r, adminCookieSecure, "error"),
			Notice:            popAdminDonationFlash(w, r, adminCookieSecure, "notice"),
			FilterStatus:      filterStatus,
			FilterVisibility:  filterVisibility,
			FilterSpam:        filterSpam,
			FilterTest:        filterTest,
			FilterProjectSlug: filterProjectSlug,
			FilterHasActive:   filterStatus != "" || filterVisibility != "" || filterSpam != "" || filterTest != "" || filterProjectSlug != "" || searchQuery != "",
			SearchQuery:       searchQuery,
			CSRFToken:         csrfToken(w, r, adminSessionSecret, adminCookieSecure),
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
			if donation.IsTest {
				page.TestCount++
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin_donations.html", page); err != nil {
			slog.Error("render admin donations", "error", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /admin/donations/filters", func(w http.ResponseWriter, r *http.Request) {
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
		setAdminDonationFilters(w, r, adminCookieSecure)
		http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /admin/donations/filters/reset", func(w http.ResponseWriter, r *http.Request) {
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
		clearAdminDonationFilters(w, adminCookieSecure)
		http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
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
	mux.HandleFunc("POST /admin/project-updates", func(w http.ResponseWriter, r *http.Request) {
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

		projectID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("project_id")), 10, 64)
		if err != nil || projectID <= 0 {
			http.Redirect(w, r, "/admin/updates?error="+url.QueryEscape("Project is required")+"&update_title="+url.QueryEscape(strings.TrimSpace(r.FormValue("title")))+"&update_body="+url.QueryEscape(strings.TrimSpace(r.FormValue("body"))), http.StatusSeeOther)
			return
		}
		title := strings.TrimSpace(r.FormValue("title"))
		body := strings.TrimSpace(r.FormValue("body"))
		if title == "" || body == "" {
			http.Redirect(w, r, "/admin/updates?error="+url.QueryEscape("Update title and body are required")+"&update_project_id="+url.QueryEscape(strconv.FormatInt(projectID, 10))+"&update_title="+url.QueryEscape(title)+"&update_body="+url.QueryEscape(body), http.StatusSeeOther)
			return
		}
		if err := db.CreateProjectUpdate(r.Context(), projectID, title, body); err != nil {
			slog.Error("create project update", "error", err, "project_id", projectID)
			http.Redirect(w, r, "/admin/updates?error="+url.QueryEscape("Update publish failed")+"&update_project_id="+url.QueryEscape(strconv.FormatInt(projectID, 10))+"&update_title="+url.QueryEscape(title)+"&update_body="+url.QueryEscape(body), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/updates?notice="+url.QueryEscape("Project update published"), http.StatusSeeOther)
	})
	mux.HandleFunc("POST /admin/project-updates/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		if err != nil || id <= 0 {
			http.Error(w, "invalid update id", http.StatusBadRequest)
			return
		}

		action := strings.TrimSpace(r.FormValue("action"))
		if action == "delete" {
			if err := db.DeleteProjectUpdate(r.Context(), id); err != nil {
				slog.Error("delete project update", "error", err, "id", id)
				http.Redirect(w, r, "/admin/updates?error="+url.QueryEscape("Update delete failed"), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/admin/updates?notice="+url.QueryEscape("Project update deleted"), http.StatusSeeOther)
			return
		}

		projectID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("project_id")), 10, 64)
		if err != nil || projectID <= 0 {
			http.Redirect(w, r, "/admin/updates?error="+url.QueryEscape("Project is required")+"&update_edit="+url.QueryEscape(strconv.FormatInt(id, 10))+"&update_title="+url.QueryEscape(strings.TrimSpace(r.FormValue("title")))+"&update_body="+url.QueryEscape(strings.TrimSpace(r.FormValue("body"))), http.StatusSeeOther)
			return
		}
		title := strings.TrimSpace(r.FormValue("title"))
		body := strings.TrimSpace(r.FormValue("body"))
		if title == "" || body == "" {
			http.Redirect(w, r, "/admin/updates?error="+url.QueryEscape("Update title and body are required")+"&update_edit="+url.QueryEscape(strconv.FormatInt(id, 10))+"&update_project_id="+url.QueryEscape(strconv.FormatInt(projectID, 10))+"&update_title="+url.QueryEscape(title)+"&update_body="+url.QueryEscape(body), http.StatusSeeOther)
			return
		}
		if err := db.UpdateProjectUpdate(r.Context(), id, projectID, title, body); err != nil {
			slog.Error("update project update", "error", err, "id", id, "project_id", projectID)
			http.Redirect(w, r, "/admin/updates?error="+url.QueryEscape("Update save failed")+"&update_edit="+url.QueryEscape(strconv.FormatInt(id, 10))+"&update_project_id="+url.QueryEscape(strconv.FormatInt(projectID, 10))+"&update_title="+url.QueryEscape(title)+"&update_body="+url.QueryEscape(body), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/updates?notice="+url.QueryEscape("Project update saved"), http.StatusSeeOther)
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
		if action == "save_note" {
			if err := db.UpdateDonationModerationNote(r.Context(), id, r.FormValue("moderation_note")); err != nil {
				slog.Error("update donation moderation note", "error", err, "id", id)
				setAdminDonationFlash(w, adminCookieSecure, "error", "Note save failed")
				http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
				return
			}
			setAdminDonationFlash(w, adminCookieSecure, "notice", "Donation note saved")
			http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
			return
		}
		if action == "refresh_status" {
			donation, err := db.FindDonationByID(r.Context(), id)
			if err != nil {
				slog.Error("find donation for refresh", "error", err, "id", id)
				setAdminDonationFlash(w, adminCookieSecure, "error", "Donation not found")
				http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
				return
			}
			if err := refreshDonationStatus(r.Context(), db, pakasirClientFromEnv(), donation); err != nil {
				slog.Error("refresh donation status", "error", err, "id", id)
				setAdminDonationFlash(w, adminCookieSecure, "error", "Refresh failed")
				http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
				return
			}
			if updated, err := db.FindDonationByID(r.Context(), id); err == nil {
				if err := notifyAdminDonationPaid(r.Context(), db, adminMailer, adminEmail, publicBaseURL, updated); err != nil {
					slog.Error("notify admin donation paid", "error", err, "id", id)
				}
			}
			setAdminDonationFlash(w, adminCookieSecure, "notice", "Donation status refreshed")
			http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
			return
		}

		if err := db.UpdateDonationModeration(r.Context(), id, action); err != nil {
			slog.Error("update donation moderation", "error", err, "id", id, "action", action)
			setAdminDonationFlash(w, adminCookieSecure, "error", "Update failed")
			http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
			return
		}

		setAdminDonationFlash(w, adminCookieSecure, "notice", "Donation updated")
		http.Redirect(w, r, "/admin/donations", http.StatusSeeOther)
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

	deadlineDate, err := app.NormalizeDeadlineDate(r.FormValue("deadline_date"))
	if err != nil {
		return app.Project{}, err
	}

	project := app.Project{
		Title:        strings.TrimSpace(r.FormValue("title")),
		Slug:         strings.TrimSpace(r.FormValue("slug")),
		Summary:      strings.TrimSpace(r.FormValue("summary")),
		Description:  strings.TrimSpace(r.FormValue("description")),
		Status:       strings.TrimSpace(r.FormValue("status")),
		Goal:         goal,
		Accent:       strings.TrimSpace(r.FormValue("accent")),
		RepoURL:      strings.TrimSpace(r.FormValue("repo_url")),
		DemoURL:      strings.TrimSpace(r.FormValue("demo_url")),
		DeadlineDate: deadlineDate,
		IsActive:     r.FormValue("is_active") == "on",
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
		base = defaultLocalBaseURL
	}
	return base + "/admin/login/verify#token=" + url.QueryEscape(token)
}

func adminRateLimitKey(r *http.Request, value string) string {
	return clientIP(r) + "|" + strings.ToLower(strings.TrimSpace(value))
}

const (
	adminDonationFiltersCookie = "admin_donations_filters"
	adminDonationNoticeCookie  = "admin_donations_notice"
	adminDonationErrorCookie   = "admin_donations_error"
)

func adminDonationFilters(r *http.Request) url.Values {
	values := url.Values{}
	cookie, err := r.Cookie(adminDonationFiltersCookie)
	if err != nil {
		return values
	}
	stored, err := url.ParseQuery(cookie.Value)
	if err != nil {
		return values
	}
	for _, key := range []string{"q", "status", "visibility", "spam", "test", "project"} {
		value := strings.TrimSpace(stored.Get(key))
		if value != "" {
			values.Set(key, value)
		}
	}
	return values
}

func setAdminDonationFilters(w http.ResponseWriter, r *http.Request, secure bool) {
	values := url.Values{}
	for _, key := range []string{"q", "status", "visibility", "spam", "test", "project"} {
		value := strings.TrimSpace(r.FormValue(key))
		if value != "" {
			values.Set(key, value)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminDonationFiltersCookie,
		Value:    values.Encode(),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 30,
	})
}

func clearAdminDonationFilters(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminDonationFiltersCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func setAdminDonationFlash(w http.ResponseWriter, secure bool, kind, value string) {
	name := adminDonationNoticeCookie
	if kind == "error" {
		name = adminDonationErrorCookie
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    url.QueryEscape(strings.TrimSpace(value)),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   20,
	})
}

func popAdminDonationFlash(w http.ResponseWriter, r *http.Request, secure bool, kind string) string {
	name := adminDonationNoticeCookie
	if kind == "error" {
		name = adminDonationErrorCookie
	}
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	value, err := url.QueryUnescape(cookie.Value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func isDevelopmentEnv(appEnv string) bool {
	return strings.EqualFold(strings.TrimSpace(appEnv), "development")
}

func isProductionEnv(appEnv string) bool {
	return strings.EqualFold(strings.TrimSpace(appEnv), "production")
}

func notifyAdminDonationPaid(ctx context.Context, db *store.Store, adminMailer mailer.Mailer, adminEmail, publicBaseURL string, donation app.Donation) error {
	if strings.TrimSpace(adminEmail) == "" || donation.Status != "paid" {
		return nil
	}
	notified, err := db.DonationAdminNotified(ctx, donation.ID)
	if err != nil {
		return err
	}
	if notified {
		return nil
	}
	adminURL := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/") + "/admin/donations"
	if strings.TrimSpace(adminURL) == "/admin/donations" {
		adminURL = defaultLocalBaseURL + "/admin/donations"
	}
	if err := adminMailer.SendAdminDonationPaid(adminEmail, donation, adminURL); err != nil {
		return err
	}
	return db.MarkDonationAdminNotified(ctx, donation.ID)
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

func homeMeta(publicBaseURL string, r *http.Request, data app.PageData) app.MetaData {
	baseURL := canonicalBaseURL(publicBaseURL, r)
	return app.MetaData{
		Title:        data.Builder.Name + " - project support",
		Description:  "Support ongoing projects and follow their progress in one place.",
		CanonicalURL: absoluteURL(baseURL, "/"),
		ImageURL:     absoluteURL(baseURL, staticAssetPath("og-default.png")),
		SiteName:     "donate.srmdn.com",
		Type:         "website",
	}
}

func projectsMeta(publicBaseURL string, r *http.Request, totalProjects int) app.MetaData {
	baseURL := canonicalBaseURL(publicBaseURL, r)
	description := "Browse ongoing projects and see their latest progress."
	if totalProjects > 0 {
		description = strconv.Itoa(totalProjects) + " ongoing projects with funding progress and recent updates."
	}
	return app.MetaData{
		Title:        "Projects - donate.srmdn.com",
		Description:  description,
		CanonicalURL: absoluteURL(baseURL, "/projects"),
		ImageURL:     absoluteURL(baseURL, staticAssetPath("og-default.png")),
		SiteName:     "donate.srmdn.com",
		Type:         "website",
	}
}

func projectMeta(publicBaseURL string, r *http.Request, builder app.Builder, project app.Project) app.MetaData {
	baseURL := canonicalBaseURL(publicBaseURL, r)
	description := strings.TrimSpace(project.Summary)
	if description == "" {
		description = strings.TrimSpace(project.Description)
	}
	if description == "" {
		description = "Project details, funding progress, and recent updates."
	}
	return app.MetaData{
		Title:        project.Title + " - " + builder.Name,
		Description:  description,
		CanonicalURL: absoluteURL(baseURL, "/projects/"+project.Slug),
		ImageURL:     absoluteURL(baseURL, staticAssetPath("og-default.png")),
		SiteName:     "donate.srmdn.com",
		Type:         "website",
	}
}

func canonicalBaseURL(publicBaseURL string, r *http.Request) string {
	baseURL := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if baseURL != "" {
		return baseURL
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.ToLower(strings.Split(forwarded, ",")[0])
	}
	return scheme + "://" + r.Host
}

func absoluteURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return path
	}
	if path == "" {
		return baseURL
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return baseURL + path
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
