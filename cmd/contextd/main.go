package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"company-context/internal/catalog"
	"company-context/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		slog.Error("contextd stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	content := flag.String("content", "./content", "Local directory containing context/ and skills/")
	remote := flag.String("git", "", "Optional Git remote URL or path; enables Git mode")
	branch := flag.String("branch", "main", "Git branch to fetch")
	gitRoot := flag.String("git-root", "content", "Content directory within the Git repository (or .)")
	state := flag.String("state", "./.state", "Private Git/cache directory")
	refresh := flag.Duration("refresh", 15*time.Minute, "Content refresh interval")
	timeout := flag.Duration("sync-timeout", 30*time.Second, "Timeout for each content refresh")
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	transport := flag.String("transport", "http", "Transport: http or stdio")
	check := flag.Bool("check", false, "Validate content, print status JSON, and exit")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *refresh <= 0 || *timeout <= 0 {
		return errors.New("refresh and sync-timeout must be positive")
	}
	if *transport != "http" && *transport != "stdio" {
		return errors.New("transport must be http or stdio")
	}
	token := os.Getenv("CONTEXT_TOKEN")
	if !*check && *transport == "http" {
		host, _, err := net.SplitHostPort(*listen)
		if err != nil {
			return fmt.Errorf("invalid listen address: %w", err)
		}
		ip := net.ParseIP(host)
		// Require a literal loopback address when unauthenticated.
		if token == "" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("set CONTEXT_TOKEN to bind beyond a literal loopback address")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var source catalog.Source = catalog.LocalSource{Dir: *content}
	cachePath, sourceKey := "", ""
	if *remote != "" {
		stateDir, err := filepath.Abs(*state)
		if err != nil {
			return err
		}
		key := sha256.Sum256([]byte(*remote + "\x00" + *branch + "\x00" + *gitRoot))
		sourceKey = hex.EncodeToString(key[:])
		cacheDir := filepath.Join(stateDir, sourceKey)
		source = catalog.GitSource{Remote: *remote, Branch: *branch, Root: *gitRoot, CacheDir: filepath.Join(cacheDir, "git")}
		cachePath = filepath.Join(cacheDir, "snapshot.json")
	}
	store := catalog.NewStore(source, cachePath, sourceKey)
	syncOnce := func() error {
		syncCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return store.Refresh(syncCtx)
	}
	if err := syncOnce(); err != nil {
		if *check {
			return err
		}
		if cacheErr := store.RestoreCache(); cacheErr != nil {
			return fmt.Errorf("initial refresh failed: %w (no usable cache)", err)
		}
		slog.Warn("starting with cached content", "error", err)
	}
	if *check {
		return json.NewEncoder(os.Stdout).Encode(store.Status())
	}
	go func() {
		ticker := time.NewTicker(*refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := syncOnce(); err != nil {
					slog.Warn("content refresh failed; keeping previous snapshot", "error", err)
				}
			}
		}
	}()
	slog.Info("content ready", "revision", store.Status().Revision, "documents", store.Status().Documents)
	if *transport == "stdio" {
		err := server.MCP(store).Run(ctx, &mcp.StdioTransport{})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	srv := &http.Server{
		Addr: *listen, Handler: server.HTTP(store, token),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("HTTP listening", "address", *listen, "mcp", "/mcp")
	err := srv.ListenAndServe()
	stop()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
