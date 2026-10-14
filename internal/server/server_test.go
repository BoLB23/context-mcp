package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"company-context/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testStore(t *testing.T) *catalog.Store {
	t.Helper()
	s := catalog.NewStore(catalog.LocalSource{Dir: "../../content"}, "", "")
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHTTP(t *testing.T) {
	h := HTTP(testStore(t), "secret")
	tests := []struct {
		path, token string
		code        int
		contains    string
	}{
		{"/healthz", "", 200, `"alive":true`},
		{"/readyz", "", 200, `"ready":true`},
		{"/api/catalog", "", 401, "bearer token required"},
		{"/api/catalog", "wrong", 401, "bearer token required"},
		{"/api/catalog?kind=skill", "secret", 200, "investigate-alert/SKILL.md"},
		{"/api/catalog?limit=1", "secret", 200, "next_offset"},
		{"/api/catalog?limit=999", "secret", 400, "limit"},
		{"/api/catalog?offset=bad", "secret", 400, "offset"},
		{"/api/search?q=architecture", "secret", 200, "context/overview.md"},
		{"/api/content?path=context/overview.md", "secret", 200, "revision"},
		{"/content/skills/investigate-alert/SKILL.md", "secret", 200, "name: investigate-alert"},
		{"/api/content?path=../go.mod", "secret", 404, "content not found"},
		{"/api/content?path=/etc/passwd", "secret", 404, "content not found"},
		{"/content/go.mod", "secret", 404, "content not found"},
		{"/status", "secret", 200, `"ready":true`},
	}
	for _, tt := range tests {
		t.Run(tt.path+"/"+tt.token, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://127.0.0.1"+tt.path, nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.code || !strings.Contains(w.Body.String(), tt.contains) {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHTTPOriginHostAndReadiness(t *testing.T) {
	h := HTTP(testStore(t), "")
	for _, tt := range []struct {
		host, origin string
		code         int
	}{
		{"localhost:8080", "", 200},
		{"[::1]:8080", "", 200},
		{"attacker.example", "", 403},
		{"127.0.0.1:8080", "https://attacker.example", 403},
	} {
		req := httptest.NewRequest("GET", "http://localhost/api/catalog", nil)
		req.Host = tt.host
		req.Header.Set("Origin", tt.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tt.code {
			t.Fatalf("%+v: status %d", tt, w.Code)
		}
	}
	empty := HTTP(catalog.NewStore(catalog.LocalSource{Dir: t.TempDir()}, "", ""), "")
	w := httptest.NewRecorder()
	empty.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/readyz", nil))
	if w.Code != 503 {
		t.Fatalf("empty catalog reported ready: %d", w.Code)
	}
}

func TestMCPOverHTTP(t *testing.T) {
	ts := httptest.NewServer(HTTP(testStore(t), "test-token"))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: "test-token"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 2 {
		t.Fatalf("tools: %+v %v", list, err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search_content", Arguments: map[string]any{"kind": "skill"}})
	if err != nil || result.IsError {
		t.Fatalf("search: %+v %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var found catalog.ListResult
	if err := json.Unmarshal(raw, &found); err != nil {
		t.Fatal(err)
	}
	if found.Total != 1 || found.Revision == "" {
		t.Fatalf("structured output: %s", raw)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "read_content", Arguments: map[string]any{"path": found.Items[0].Path}})
	if err != nil || result.IsError {
		t.Fatalf("read: %+v %v", result, err)
	}
	raw, _ = json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), "Starter skill") {
		t.Fatalf("missing skill content: %s", raw)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "read_content", Arguments: map[string]any{"path": "../../secret"}})
	if err != nil || !result.IsError {
		t.Fatalf("missing path should be a tool error: %+v %v", result, err)
	}
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(c)
}

func TestRawMarkdownMatchesMCP(t *testing.T) {
	s := testStore(t)
	ts := httptest.NewServer(HTTP(s, ""))
	defer ts.Close()
	res, err := http.Get(ts.URL + "/content/context/overview.md")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.Read("context/overview.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != doc.Document.Content || res.Header.Get("X-Content-Revision") != doc.Revision {
		t.Fatal("HTTP and catalog diverged")
	}
}
