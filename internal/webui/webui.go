package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var embedded embed.FS

func Index(w http.ResponseWriter, r *http.Request) {
	data, err := embedded.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "web UI is unavailable", http.StatusInternalServerError)
		return
	}
	secureHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func Assets() http.Handler {
	static, err := fs.Sub(embedded, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		w.Header().Set("Cache-Control", "no-cache")
		http.FileServer(http.FS(static)).ServeHTTP(w, r)
	})
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
