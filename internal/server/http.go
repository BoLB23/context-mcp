package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"company-context/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func HTTP(store *catalog.Store, token string) http.Handler {
	mux := http.NewServeMux()
	mcpServer := MCP(store)
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.Status())
	})
	list := func(w http.ResponseWriter, r *http.Request) {
		q := catalog.Query{Kind: r.URL.Query().Get("kind")}
		if r.URL.Path == "/api/search" {
			q.Query = r.URL.Query().Get("q")
		}
		var err error
		if v := r.URL.Query().Get("limit"); v != "" {
			q.Limit, err = strconv.Atoi(v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "limit must be an integer")
				return
			}
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			q.Offset, err = strconv.Atoi(v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "offset must be an integer")
				return
			}
		}
		result, err := store.List(q)
		if err != nil {
			catalogError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
	mux.HandleFunc("GET /api/catalog", list)
	mux.HandleFunc("GET /api/search", list)
	mux.HandleFunc("GET /api/content", func(w http.ResponseWriter, r *http.Request) {
		result, err := store.Read(r.URL.Query().Get("path"))
		if err != nil {
			catalogError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /content/{path...}", func(w http.ResponseWriter, r *http.Request) {
		result, err := store.Read(r.PathValue("path"))
		if err != nil {
			catalogError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("X-Content-Revision", result.Revision)
		w.Header().Set("ETag", `"`+result.Document.SHA256+`"`)
		if len(result.Warnings) > 0 {
			w.Header().Set("X-Content-Warning", result.Warnings[0])
		}
		_, _ = w.Write([]byte(result.Document.Content))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"name": "company-context", "mcp": "/mcp",
			"catalog": "/api/catalog", "search": "/api/search?q=...",
			"read":     "/api/content?path=context/overview.md",
			"markdown": "/content/context/overview.md", "status": "/status",
		})
	})
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		// This MVP is a programmatic endpoint. Browser origins are not enabled.
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "browser origins are not enabled")
			return
		}
		// Protect unauthenticated loopback endpoints from DNS rebinding.
		if token == "" && !loopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "invalid host")
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/healthz" {
			writeJSON(w, http.StatusOK, map[string]bool{"alive": true})
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/readyz" {
			ready := store.Status().Ready
			code := http.StatusOK
			if !ready {
				code = http.StatusServiceUnavailable
			}
			writeJSON(w, code, map[string]bool{"ready": ready})
			return
		}
		if token != "" {
			auth := r.Header.Get("Authorization")
			got := sha256.Sum256([]byte(strings.TrimPrefix(auth, "Bearer ")))
			if !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="company-context"`)
				writeError(w, http.StatusUnauthorized, "bearer token required")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func catalogError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, catalog.ErrNotFound) {
		code = http.StatusNotFound
	}
	if errors.Is(err, catalog.ErrNotReady) {
		code = http.StatusServiceUnavailable
	}
	writeError(w, code, err.Error())
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
