package learn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/tool"
)

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitWithEnv(ctx, dir, nil, args...)
}

func gitWithEnv(ctx context.Context, dir string, extra []string, args ...string) ([]byte, error) {
	// Disable repository hooks for worktree checkout as well as commit.
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-C", dir, "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(e string) bool {
		key, _, _ := strings.Cut(e, "=")
		return slices.Contains([]string{"GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_REFLOG_ACTION"}, strings.ToUpper(key))
	})
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	cmd.Env = append(cmd.Env, extra...)
	cmd.WaitDelay = time.Second
	data, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if e, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(e.Stderr)))
		}
	}
	return data, err
}

func exitCode(err error) int {
	if e, ok := errors.AsType[*exec.ExitError](err); ok {
		return e.ExitCode()
	}
	return -1
}

func samePath(a, b string) bool { rel, err := filepath.Rel(a, b); return err == nil && rel == "." }

var errNotRepository = errors.New("directory is not in a git repository")
var errRepositoryRead = errors.New("cannot read run repository")

func repository(ctx context.Context, dir string, roots []string) (string, error) {
	if dir == "" {
		return "", tool.ErrOutsideRoots
	}
	root, rel, err := tool.WithinRoots(dir, roots)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(root, rel)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errRepositoryRead, err)
	}
	if !info.IsDir() {
		return "", errNotRepository
	}
	top, err := git(ctx, resolved, "rev-parse", "--show-toplevel")
	if e, ok := errors.AsType[*exec.ExitError](err); ok && (bytes.Contains(e.Stderr, []byte("not a git repository")) || bytes.Contains(e.Stderr, []byte("must be run in a work tree"))) {
		return "", errNotRepository
	}
	if err != nil {
		return "", err
	}
	root, rel, err = tool.WithinRoots(strings.TrimRight(string(top), "\r\n"), roots)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, rel), nil
}

func (s Store) checkRepo(ctx context.Context, p Proposal) error {
	top, err := repository(ctx, p.Repo, s.Roots)
	if err != nil {
		return err
	}
	if !samePath(top, p.Repo) {
		return fmt.Errorf("proposal repository is no longer a git top level")
	}
	return nil
}

func revision(ctx context.Context, repo, ref string) (string, error) {
	out, err := git(ctx, repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !gitHash.MatchString(sha) {
		return "", fmt.Errorf("invalid git commit")
	}
	return sha, nil
}

func branchTip(ctx context.Context, p Proposal) (string, error) {
	ref := "refs/heads/" + p.branch()
	out, err := git(ctx, p.Repo, "rev-parse", "--verify", "--quiet", ref)
	if e, ok := errors.AsType[*exec.ExitError](err); ok && e.ExitCode() == 1 {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}

func hasTreePath(ctx context.Context, repo, ref, path string) (bool, error) {
	if _, err := git(ctx, repo, "cat-file", "-e", ref+":"+path); err == nil {
		return true, nil
	}
	// ls-tree distinguishes a missing path from an unreadable tree.
	out, err := git(ctx, repo, "ls-tree", "-z", ref, "--", path)
	return len(out) > 0, err
}

func removeBranch(ctx context.Context, p Proposal, expected string) (bool, error) {
	tip, err := branchTip(ctx, p)
	if err != nil || tip == "" || tip != expected {
		return false, err
	}
	ref := "refs/heads/" + p.branch()
	worktrees, err := git(ctx, p.Repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	if slices.Contains(strings.Split(string(worktrees), "\x00"), "branch "+ref) {
		return false, nil
	}
	// Force deletion must compare the old object ID under Git's ref lock.
	_, err = git(ctx, p.Repo, "update-ref", "--no-deref", "-d", ref, expected)
	if err != nil {
		tip, checkErr := branchTip(ctx, p)
		if checkErr == nil && tip != expected {
			return false, nil
		}
	}
	return err == nil, err
}

func writeSkill(dir string, skill Skill) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	// A tracked symlink at either parent must not redirect the candidate write.
	path := "."
	for _, part := range strings.Split(skill.path(), "/") {
		path = filepath.Join(path, part)
		if err := root.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill parent %s must be a directory", path)
		}
	}
	f, err := root.OpenFile(filepath.Join(path, "SKILL.md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(skill.render())
	return errors.Join(err, f.Close())
}

func verifyBlob(ctx context.Context, p Proposal, commit string) error {
	path := commit + ":" + p.Skill.path() + "/SKILL.md"
	expected := p.Skill.render()
	size, err := git(ctx, p.Repo, "cat-file", "-s", path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(size)) != strconv.Itoa(len(expected)) {
		return fmt.Errorf("SKILL.md differs from the proposed skill")
	}
	data, err := git(ctx, p.Repo, "show", path)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expected) {
		return fmt.Errorf("SKILL.md differs from the proposed skill")
	}
	return nil
}

// A missing checkout can leave its registration behind even after Git refuses removal.
func removeMeasureAdmin(ctx context.Context, repo, dir, admin string) error {
	common, err := git(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	worktrees := filepath.Join(filepath.FromSlash(strings.TrimRight(string(common), "\r\n")), "worktrees")
	if !filepath.IsAbs(admin) || filepath.Clean(admin) != admin || !samePath(filepath.Dir(admin), worktrees) {
		return fmt.Errorf("measurement admin directory is outside the repository worktrees folder")
	}
	for _, path := range []string{worktrees, admin} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("measurement admin directory changed outside Ada")
		}
	}
	root, err := os.OpenRoot(worktrees)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(admin)
	link := filepath.Join(name, "gitdir")
	info, err := root.Lstat(link)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("invalid measurement gitdir file")
	}
	data, err := root.ReadFile(link)
	if err != nil {
		return err
	}
	target := filepath.FromSlash(strings.TrimRight(string(data), "\r\n"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(admin, target)
	}
	if !samePath(target, filepath.Join(dir, ".git")) {
		return fmt.Errorf("measurement admin directory belongs to another worktree")
	}
	return root.RemoveAll(name)
}
