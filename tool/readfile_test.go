package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadFileScopeAndSize(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "root-adjacent.txt")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		outside:                      "private",
		filepath.Join(root, "notes"): "inside",
		filepath.Join(root, "exact"): strings.Repeat("x", 256<<10),
		filepath.Join(root, "large"): strings.Repeat("x", (256<<10)+1),
	} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := ReadFile{Roots: []string{root}}
	for _, tc := range []struct{ path, result, errorText string }{
		{filepath.Join(root, "notes"), "inside", ""},
		{filepath.Join(root, "exact"), strings.Repeat("x", 256<<10), ""},
		{root + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(outside), "", "outside"},
		{filepath.Join(root, "large"), "", "256 KiB"},
		{root, "", "directory"},
	} {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"path": tc.path})
			got, err := read.Run(t.Context(), input)
			if tc.errorText == "" && (err != nil || got != tc.result) ||
				tc.errorText != "" && (err == nil || !strings.Contains(err.Error(), tc.errorText)) {
				t.Fatalf("ReadFile returned %d bytes, %v", len(got), err)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(root, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Skip(err)
		}
		input, _ := json.Marshal(map[string]string{"path": link})
		if _, err := read.Run(t.Context(), input); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("ReadFile returned %v", err)
		}
	})
}

func TestReadFileOutsideAndMissingAreIndistinguishable(t *testing.T) {
	root, outside := t.TempDir(), filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	read := ReadFile{Roots: []string{root}}
	for _, path := range []string{outside, outside + "-missing"} {
		input, _ := json.Marshal(map[string]string{"path": path})
		if _, err := read.Run(t.Context(), input); err == nil || err.Error() != "read file: path is outside the configured roots or does not exist" {
			t.Errorf("Run(%q) = %v", path, err)
		}
	}
}

func TestReadFileWindowsRootCase(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows paths are case-insensitive")
	}
	root := filepath.Join(t.TempDir(), "MiXeD")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "notes")
	if err := os.WriteFile(path, []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"path": path})
	if got, err := (ReadFile{Roots: []string{strings.ToUpper(root)}}).Run(t.Context(), input); err != nil || got != "inside" {
		t.Fatalf("Run = %q, %v", got, err)
	}
}
