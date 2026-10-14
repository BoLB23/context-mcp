package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Status struct {
	Ready                 bool      `json:"ready"`
	Revision              string    `json:"revision,omitempty"`
	Documents             int       `json:"documents"`
	LoadedAt              time.Time `json:"loaded_at"`
	LastCheckedAt         time.Time `json:"last_checked_at"`
	LastSuccessfulRefresh time.Time `json:"last_successful_refresh"`
	LastError             string    `json:"last_error,omitempty"`
}

// Store holds one immutable snapshot. A single lock publishes its metadata and
// documents together; queries do their work after releasing the lock.
type Store struct {
	source    Source
	cachePath string
	sourceKey string
	refreshMu sync.Mutex
	mu        sync.RWMutex
	snapshot  *Snapshot
	status    Status
}

func NewStore(source Source, cachePath, sourceKey string) *Store {
	return &Store{source: source, cachePath: cachePath, sourceKey: sourceKey}
}

func (s *Store) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	c, err := s.source.Load(ctx)
	var next *Snapshot
	if err == nil {
		next, err = Build(c)
	}
	now := time.Now().UTC()
	if err == nil {
		s.mu.RLock()
		if s.snapshot != nil && s.snapshot.Revision == next.Revision {
			next = s.snapshot
		}
		s.mu.RUnlock()
		if s.cachePath != "" {
			err = saveCache(s.cachePath, diskCache{SourceKey: s.sourceKey, Candidate: c, LoadedAt: next.LoadedAt, LastSuccessfulRefresh: now})
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.LastCheckedAt = now
	if err != nil {
		s.status.LastError = err.Error()
		return err
	}
	s.snapshot = next
	s.status = Status{Ready: true, Revision: next.Revision, Documents: len(next.paths),
		LoadedAt: next.LoadedAt, LastCheckedAt: now, LastSuccessfulRefresh: now}
	return nil
}

func (s *Store) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *Store) current() (*Snapshot, Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snapshot == nil {
		return nil, Metadata{}, ErrNotReady
	}
	meta := Metadata{Revision: s.snapshot.Revision, LoadedAt: s.snapshot.LoadedAt, LastSuccessfulRefresh: s.status.LastSuccessfulRefresh}
	if s.status.LastError != "" {
		meta.Warnings = []string{"Refresh failed; serving the last valid snapshot. See /status for details."}
	}
	return s.snapshot, meta, nil
}

func (s *Store) List(q Query) (ListResult, error) {
	snap, meta, err := s.current()
	if err != nil {
		return ListResult{}, err
	}
	r, err := snap.List(q)
	r.Metadata = meta
	return r, err
}

func (s *Store) Read(p string) (ReadResult, error) {
	snap, meta, err := s.current()
	if err != nil {
		return ReadResult{}, err
	}
	d, err := snap.Read(p)
	return ReadResult{Metadata: meta, Document: d}, err
}

type diskCache struct {
	SourceKey             string    `json:"source_key"`
	Candidate             Candidate `json:"candidate"`
	LoadedAt              time.Time `json:"loaded_at"`
	LastSuccessfulRefresh time.Time `json:"last_successful_refresh"`
}

// RestoreCache is used at startup only after a failed refresh. It never clears
// that failure, so callers can distinguish cached guidance from a fresh check.
func (s *Store) RestoreCache() error {
	if s.cachePath == "" {
		return errors.New("no persistent cache configured")
	}
	f, err := os.Open(s.cachePath)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 128<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 128<<20 {
		return errors.New("snapshot cache is too large")
	}
	var c diskCache
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	if c.SourceKey != s.sourceKey {
		return errors.New("cached snapshot belongs to another source")
	}
	next, err := Build(c.Candidate)
	if err != nil {
		return fmt.Errorf("invalid cached snapshot: %w", err)
	}
	next.LoadedAt = c.LoadedAt
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = next
	s.status.Ready = true
	s.status.Revision = next.Revision
	s.status.Documents = len(next.paths)
	s.status.LoadedAt = c.LoadedAt
	s.status.LastSuccessfulRefresh = c.LastSuccessfulRefresh
	return nil
}

func saveCache(p string, c diskCache) error {
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), "snapshot-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
