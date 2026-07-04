package main

import (
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/srmdn/donation/internal/app"
	"github.com/srmdn/donation/internal/store"
)

//go:embed web/templates/*.html web/static/*
var assets embed.FS

func main() {
	addr := env("ADDR", "127.0.0.1:8094")
	dbPath := env("DB_PATH", "data/donation.db")
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
		data, err := db.PageData(r.Context())
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
	mux.HandleFunc("GET /projects/{slug}", func(w http.ResponseWriter, r *http.Request) {
		data, err := db.PageData(r.Context())
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
		amount, err := strconv.Atoi(strings.TrimSpace(r.FormValue("amount")))
		if err != nil {
			http.Error(w, "invalid amount", http.StatusBadRequest)
			return
		}
		if slug == "" {
			http.Error(w, "missing project", http.StatusBadRequest)
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
		data, err := db.PageData(r.Context())
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
		return "Rp0"
	}
	s := strconv.Itoa(amount)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return "Rp" + strings.Join(parts, ".")
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
