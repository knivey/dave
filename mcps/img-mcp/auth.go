package main

import (
	"net/http"
)

const apiKeyHeader = "X-API-Key"

func apiKeyMiddleware(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(apiKeyHeader) != key {
				logger.Warn("auth failed", "remote_addr", r.RemoteAddr, "path", r.URL.Path, "method", r.Method)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
