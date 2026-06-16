package main

import (
	"net/http"
	"os"
	"strings"
)

const (
	defaultAllowedOrigins = "http://localhost:8080"
	corsAllowMethods      = "POST, GET, OPTIONS, PUT, DELETE"
	corsAllowHeaders      = "Content-Type, Authorization, X-API-Key"
)

type corsConfig struct {
	allowed map[string]struct{}
}

func loadCORSConfig() *corsConfig {
	raw := strings.TrimSpace(os.Getenv("ALLOWED_ORIGINS"))
	if raw == "" {
		raw = defaultAllowedOrigins
	}

	allowed := make(map[string]struct{})
	for _, origin := range strings.Split(raw, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		allowed[origin] = struct{}{}
	}

	return &corsConfig{allowed: allowed}
}

func (c *corsConfig) isAllowed(origin string) bool {
	_, ok := c.allowed[origin]
	return ok
}

// CORSMiddleware habilita requisições cross-origin (ex.: frontend Vercel → backend Render).
// Deve ser a camada mais externa do servidor HTTP.
func CORSMiddleware(cfg *corsConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && cfg.isAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}

		w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
		w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
