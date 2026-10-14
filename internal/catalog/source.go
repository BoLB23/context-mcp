package catalog

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Source interface {
	Load(context.Context) (Candidate, error)
}

type LocalSource struct{ Dir string }

func (s LocalSource) Load(ctx context.Context) (Candidate, error) {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return Candidate{}, fmt.Errorf("open content directory: %w", err)
	}
	defer root.Close()
	c := Candidate{}
	total := 0
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (strings.HasPrefix(d.Name(), ".") || (p != "context" && p != "skills" && !strings.Contains(p, "/"))) {
				return fs.SkipDir
			}
			return nil
		}
		if KindForPath(p) == "" {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: content must be a regular file (no symlinks)", p)
		}
		f, err := root.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		// Check the opened file as well, in case it changed during the walk.
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
		if err != nil {
			return err
		}
		total += len(data)
		if len(data) > MaxFileBytes || total > MaxTotalBytes || len(c.Files) >= MaxFiles {
			return errors.New("content size or file count limit exceeded")
		}
		c.Files = append(c.Files, File{Path: p, Content: string(data)})
		return nil
	})
	return c, err
}

// GitSource fetches into a private bare repository. It reads an archive directly
// without checking out files, following symlinks, or executing repository code.
type GitSource struct {
	Remote   string
	Branch   string
	Root     string
	CacheDir string
}

func (s GitSource) Load(ctx context.Context) (Candidate, error) {
	if s.Remote == "" || s.Branch == "" || !fs.ValidPath(s.Root) || strings.ContainsAny(s.Root, ":\\") {
		return Candidate{}, errors.New("Git requires a remote, branch, and relative content root")
	}
	if _, err := git(ctx, "", "check-ref-format", "refs/heads/"+s.Branch); err != nil {
		return Candidate{}, errors.New("invalid Git branch")
	}
	if err := os.MkdirAll(s.CacheDir, 0700); err != nil {
		return Candidate{}, err
	}
	if _, err := os.Stat(filepath.Join(s.CacheDir, "HEAD")); os.IsNotExist(err) {
		if _, err := git(ctx, "", "init", "--bare", s.CacheDir); err != nil {
			return Candidate{}, err
		}
	}
	if _, err := git(ctx, s.CacheDir, "fetch", "--quiet", "--no-tags", "--depth=1", "--", s.Remote, "refs/heads/"+s.Branch); err != nil {
		return Candidate{}, fmt.Errorf("fetch failed: %w", err)
	}
	rev, err := git(ctx, s.CacheDir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return Candidate{}, err
	}
	c := Candidate{Revision: strings.TrimSpace(string(rev))}
	tree := c.Revision
	if s.Root != "." {
		tree += ":" + s.Root
	}
	archive, err := git(ctx, s.CacheDir, "archive", "--format=tar", tree)
	if err != nil {
		return Candidate{}, fmt.Errorf("read content tree failed: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	total := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Candidate{}, err
		}
		if KindForPath(h.Name) == "" {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return Candidate{}, fmt.Errorf("%s: content must be a regular file", h.Name)
		}
		if h.Size > MaxFileBytes || h.Size < 0 || int64(total)+h.Size > MaxTotalBytes || len(c.Files) >= MaxFiles {
			return Candidate{}, errors.New("content size or file count limit exceeded")
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return Candidate{}, err
		}
		total += len(data)
		c.Files = append(c.Files, File{Path: h.Name, Content: string(data)})
	}
	return c, nil
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > 32<<20-b.Len() {
		return 0, errors.New("Git output exceeds 32 MiB")
	}
	return b.Buffer.Write(p)
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.ext.allow=never", "-c", "credential.interactive=false"}
	if dir != "" {
		base = append(base, "--git-dir="+dir)
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	// Ignore inherited repository overrides; retain SSH/credential configuration.
	for _, v := range os.Environ() {
		if strings.HasPrefix(v, "GIT_DIR=") || strings.HasPrefix(v, "GIT_WORK_TREE=") ||
			strings.HasPrefix(v, "GIT_INDEX_FILE=") || strings.HasPrefix(v, "GIT_OBJECT_DIRECTORY=") ||
			strings.HasPrefix(v, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") || strings.HasPrefix(v, "GIT_TERMINAL_PROMPT=") {
			continue
		}
		cmd.Env = append(cmd.Env, v)
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	var buf boundedBuffer
	cmd.Stdout = &buf
	// Git stderr may contain credential-bearing remote URLs. Do not publish it.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return buf.Bytes(), nil
}
