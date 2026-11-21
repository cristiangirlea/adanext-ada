package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cristiangirlea/adanext-ada/provider"
)

const readFileLimit = 256 << 10

type ReadFile struct {
	Roots []string
}

func (ReadFile) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "read_file",
		Description: "Read a file inside the configured read roots, up to 256 KiB.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
	}
}

func (ReadFile) Effect() Effect { return Read }

func (t ReadFile) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var input struct{ Path string }
	if err := decodeInput(raw, &input); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, rel, err := withinRoots(input.Path, t.Roots)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	// A rooted open also confines the read if a symlink changes after validation.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("open read root: %w", err)
	}
	defer dir.Close()
	info, err := dir.Stat(rel)
	if err != nil {
		return "", fmt.Errorf("inspect file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read file: path is a directory")
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read file: path is not a regular file")
	}
	if info.Size() > readFileLimit {
		return "", fmt.Errorf("read file: file exceeds 256 KiB")
	}
	file, err := dir.Open(rel)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, readFileLimit+1))
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if len(data) > readFileLimit {
		return "", fmt.Errorf("read file: file exceeds 256 KiB")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return string(data), nil
}

func withinRoots(path string, roots []string) (rootPath, relative string, err error) {
	outsideOrMissing := errors.New("path is outside the configured roots or does not exist")
	if path == "" {
		return "", "", fmt.Errorf("path is required")
	}
	target, targetErr := resolvedPath(path)
	for _, root := range roots {
		resolved, err := resolvedPath(root)
		if err != nil {
			return "", "", fmt.Errorf("resolve configured root: %w", err)
		}
		if targetErr != nil {
			continue
		}
		// Rel compares Windows path components case-insensitively.
		rel, err := filepath.Rel(resolved, target)
		if err == nil && filepath.IsLocal(rel) {
			return resolved, rel, nil
		}
	}
	return "", "", outsideOrMissing
}

func resolvedPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
