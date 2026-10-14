// Package catalog loads a small Markdown library into immutable snapshots.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	MaxFileBytes  = 256 << 10
	MaxTotalBytes = 16 << 20
	MaxFiles      = 2000
)

var ErrNotFound = errors.New("content not found")
var ErrNotReady = errors.New("no valid content snapshot available")

type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Candidate struct {
	Revision string `json:"revision"`
	Files    []File `json:"files"`
}

type Entry struct {
	Path        string   `json:"path"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Aliases     []string `json:"aliases,omitempty"`
	SHA256      string   `json:"sha256"`
}

type Document struct {
	Entry
	Content string `json:"content"`
	body    string
}

type Snapshot struct {
	Revision string
	LoadedAt time.Time
	docs     map[string]Document
	paths    []string
}

type Metadata struct {
	Revision              string    `json:"revision"`
	LoadedAt              time.Time `json:"loaded_at"`
	LastSuccessfulRefresh time.Time `json:"last_successful_refresh"`
	Warnings              []string  `json:"warnings,omitempty"`
}

type Match struct {
	Entry
	Excerpt string `json:"excerpt,omitempty"`
	Score   int    `json:"score,omitempty"`
}

type ListResult struct {
	Metadata
	Items      []Match `json:"items"`
	Total      int     `json:"total"`
	NextOffset *int    `json:"next_offset,omitempty"`
}

type ReadResult struct {
	Metadata
	Document Document `json:"document"`
}

type Query struct {
	Query  string `json:"query,omitempty" jsonschema:"Words to search; empty lists available content"`
	Kind   string `json:"kind,omitempty" jsonschema:"Optional filter: context, skill, or reference"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Page size, default 20, maximum 100"`
	Offset int    `json:"offset,omitempty" jsonschema:"Zero-based result offset"`
}

// KindForPath is also the allowlist for published files. Other repository files
// (including source code, dotfiles, and credentials) never enter the snapshot.
func KindForPath(p string) string {
	if !fs.ValidPath(p) || strings.Contains(p, "\\") || !strings.HasSuffix(p, ".md") {
		return ""
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return ""
		}
	}
	if len(parts) >= 2 && parts[0] == "context" {
		return "context"
	}
	if len(parts) >= 3 && parts[0] == "skills" {
		if len(parts) == 3 && parts[2] == "SKILL.md" {
			return "skill"
		}
		return "reference"
	}
	return ""
}

func Build(c Candidate) (*Snapshot, error) {
	if len(c.Files) == 0 {
		return nil, errors.New("content library contains no Markdown documents")
	}
	if len(c.Files) > MaxFiles {
		return nil, errors.New("too many content files")
	}
	s := &Snapshot{Revision: c.Revision, LoadedAt: time.Now().UTC(), docs: make(map[string]Document)}
	total := 0
	for _, f := range c.Files {
		total += len(f.Content)
		if len(f.Content) > MaxFileBytes || total > MaxTotalBytes {
			return nil, errors.New("content size limit exceeded")
		}
		if _, exists := s.docs[f.Path]; exists {
			return nil, fmt.Errorf("duplicate path: %s", f.Path)
		}
		d, err := parse(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		s.docs[f.Path] = d
		s.paths = append(s.paths, f.Path)
	}
	sort.Strings(s.paths)
	for _, p := range s.paths {
		if s.docs[p].Kind == "reference" {
			parent := strings.Join(strings.Split(p, "/")[:2], "/") + "/SKILL.md"
			if _, ok := s.docs[parent]; !ok {
				return nil, fmt.Errorf("%s has no parent SKILL.md", p)
			}
		}
	}
	if s.Revision == "" {
		h := sha256.New()
		for _, p := range s.paths {
			fmt.Fprintf(h, "%d:%s%d:%s", len(p), p, len(s.docs[p].Content), s.docs[p].Content)
		}
		s.Revision = "local:" + hex.EncodeToString(h.Sum(nil))
	}
	return s, nil
}

func parse(f File) (Document, error) {
	kind := KindForPath(f.Path)
	if kind == "" {
		return Document{}, errors.New("unsupported content path")
	}
	if !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) {
		return Document{}, errors.New("content must be UTF-8 text")
	}
	body := strings.ReplaceAll(f.Content, "\r\n", "\n")
	var meta struct {
		Title       string   `yaml:"title"`
		Description string   `yaml:"description"`
		Aliases     []string `yaml:"aliases"`
		Name        string   `yaml:"name"`
	}
	if strings.HasPrefix(body, "---\n") {
		lines := strings.Split(body, "\n")
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end < 0 {
			return Document{}, errors.New("unterminated YAML frontmatter")
		}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &meta); err != nil {
			return Document{}, fmt.Errorf("invalid YAML frontmatter: %w", err)
		}
		body = strings.Join(lines[end+1:], "\n")
	}
	if kind == "skill" {
		if meta.Name != strings.Split(f.Path, "/")[1] || strings.TrimSpace(meta.Description) == "" {
			return Document{}, errors.New("skill requires a name matching its directory and a description")
		}
		if len(meta.Name) > 64 || strings.Trim(meta.Name, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
			strings.HasPrefix(meta.Name, "-") || strings.HasSuffix(meta.Name, "-") || strings.Contains(meta.Name, "--") {
			return Document{}, errors.New("skill name must use lowercase letters, digits, and single hyphens")
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if meta.Title == "" && strings.HasPrefix(line, "# ") {
			meta.Title = strings.TrimSpace(line[2:])
		}
		if meta.Description == "" && strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			meta.Description = clip(strings.TrimSpace(line), 240)
		}
	}
	if meta.Title == "" {
		meta.Title = strings.TrimSuffix(path.Base(f.Path), ".md")
	}
	sum := sha256.Sum256([]byte(f.Content))
	return Document{Entry: Entry{Path: f.Path, Kind: kind, Title: meta.Title, Description: meta.Description,
		Aliases: meta.Aliases, SHA256: hex.EncodeToString(sum[:])}, Content: f.Content, body: body}, nil
}

func (s *Snapshot) List(q Query) (ListResult, error) {
	if q.Kind != "" && q.Kind != "context" && q.Kind != "skill" && q.Kind != "reference" {
		return ListResult{}, errors.New("kind must be context, skill, or reference")
	}
	if q.Limit < 0 || q.Limit > 100 || q.Offset < 0 {
		return ListResult{}, errors.New("limit must be 0..100 and offset must be nonnegative")
	}
	if len(q.Query) > 1000 {
		return ListResult{}, errors.New("query exceeds 1000 bytes")
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	terms := strings.FieldsFunc(strings.ToLower(q.Query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' && r != '_' })
	matches := make([]Match, 0)
	for _, p := range s.paths {
		d := s.docs[p]
		if q.Kind != "" && q.Kind != d.Kind {
			continue
		}
		score := 0
		for _, term := range terms {
			if strings.Contains(strings.ToLower(d.Title), term) {
				score += 10
			}
			if strings.Contains(strings.ToLower(strings.Join(d.Aliases, " ")), term) {
				score += 10
			}
			if strings.Contains(strings.ToLower(d.Path), term) {
				score += 5
			}
			if strings.Contains(strings.ToLower(d.Description), term) {
				score += 3
			}
			if strings.Contains(strings.ToLower(d.body), term) {
				score++
			}
		}
		if len(terms) > 0 && score == 0 {
			continue
		}
		m := Match{Entry: d.Entry, Score: score}
		if len(terms) > 0 {
			m.Excerpt = excerpt(d.body, terms)
		}
		matches = append(matches, m)
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	total := len(matches)
	start := min(q.Offset, total)
	end := start + min(q.Limit, total-start)
	result := ListResult{Items: matches[start:end], Total: total}
	if end < total {
		result.NextOffset = &end
	}
	return result, nil
}

func (s *Snapshot) Read(p string) (Document, error) {
	d, ok := s.docs[p]
	if !ok {
		return Document{}, ErrNotFound
	}
	return d, nil
}

func excerpt(body string, terms []string) string {
	for _, line := range strings.Split(body, "\n") {
		for _, term := range terms {
			if strings.Contains(strings.ToLower(line), term) {
				return clip(strings.TrimSpace(line), 300)
			}
		}
	}
	return clip(strings.Join(strings.Fields(body), " "), 300)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
