package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/learn"
)

const cliSkillReply = `{"skill":{"name":"build-check","description":"Check a build","body":"Run the build gate."}}`

func init() {
	repo := os.Getenv("ADA_CLI_SKILL_REPO")
	if repo == "" || len(os.Args) < 3 {
		return
	}
	switch os.Args[1] {
	case "show":
		if len(os.Args) != 4 || os.Args[2] != "--json" || os.Args[3] != firstRun {
			os.Exit(2)
		}
		fmt.Printf(`{"record":{"task":{"dir":%q,"prompt":"build"},"result":{"status":"ok","final":"built"}}}`, repo)
	case "eval":
		args := os.Args[2:]
		if len(args) != 11 || strings.Join(args[3:10], " ") != "--paths .claude/skills/build-check --json --held-out 0.5 --seed 9" || !filepath.IsAbs(args[10]) {
			os.Exit(2)
		}
		head, e := exec.Command("git", "rev-parse", "HEAD").Output()
		if e != nil {
			os.Exit(3)
		}
		if os.Getenv("ADA_CLI_LOCK_WORKTREE") != "" {
			if err := exec.Command("git", "worktree", "lock", ".").Run(); err != nil {
				os.Exit(4)
			}
		}
		fmt.Fprintln(os.Stderr, "gate complete")
		fmt.Printf(`{"verdict":"better","head":%q,"reason":"more passing gates","baseline":{"eval":"left"},"candidate":{"eval":"right"}}`, strings.TrimSpace(string(head)))
	default:
		return
	}
	os.Exit(0)
}
func cliSkillRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "--allow-empty", "-qm", "start"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	t.Setenv("ADA_CLI_SKILL_REPO", repo)
	return repo
}
func proposeCLISkill(t *testing.T) (*knowledgeCLI, string, string) {
	t.Helper()
	k := newKnowledgeCLI(t)
	repo := cliSkillRepo(t)
	delete(k.settings, "knowledge")
	k.settings["workspace_roots"] = []string{repo}
	k.journal(t, firstRun)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}]}`, cliSkillReply)
	}))
	t.Cleanup(server.Close)
	k.settings["providers"].([]map[string]string)[0]["base_url"] = server.URL
	if out, _, err := k.invoke(t, "propose"); err != nil || out != "" {
		t.Fatal(out, err)
	}
	k.settings["learning"] = map[string]bool{"skills": true}
	out, _, err := k.invoke(t, "propose")
	if err != nil || !strings.Contains(out, "outside=true") {
		t.Fatal(out, err)
	}
	return k, repo, strings.Fields(out)[0]
}

func TestSkillCLI(t *testing.T) {
	k, _, id := proposeCLISkill(t)
	list, _, err := k.invoke(t, "list")
	if err != nil || !strings.Contains(list, "build-check") || !strings.Contains(list, "Check a build") || !strings.Contains(list, "\n    Run the build gate.\n") {
		t.Errorf("list = %q, %v", list, err)
	}
	spec := filepath.Join(k.dir, "eval.json")
	if err := os.WriteFile(spec, []byte(`{"skills":true,"repeats":2,"matrix":[{"backend":"claude"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, diag, err := k.invoke(t, "measure", id, "-spec", spec, "-held-out", "0.5", "-seed", "9")
	if err != nil || !strings.Contains(out, "more passing gates") || !strings.Contains(out, "keep it with ada learn keep "+id+" -outside") || !strings.Contains(diag, "gate complete") {
		t.Fatal(out, diag, err)
	}
	preview := "---\nname: build-check\ndescription: \"Check a build\"\n---\n\nRun the build gate.\n"
	before, _, found := strings.Cut(diag, "gate complete")
	if !found || !strings.Contains(before, preview) || !strings.Contains(before, "this skill will be loaded by the eval's claude runs\n") {
		t.Errorf("preview and progress = %q", diag)
	}
	out, _, err = k.invoke(t, "keep", id, "-outside")
	if err != nil || !strings.Contains(out, "Merge branch") || !strings.Contains(out, "hrn.skills") {
		t.Fatal(out, err)
	}
}

func TestServeReviewsSkill(t *testing.T) {
	c := newLearningChat(t, false)
	repo := cliSkillRepo(t)
	c.settings.WorkspaceRoots = []string{repo}
	c.settings.HRN.Path = os.Args[0]
	c.settings.Learning.Skills = true
	store := learn.Store{Memory: c.app.Store, Roots: []string{repo}}
	if err := store.RecordDelegation(firstRun, "another", "abc"); err != nil {
		t.Fatal(err)
	}
	c.startLearning(io.Discard)
	until := time.Now().Add(10 * time.Second)
	for {
		items, err := store.List(c.settings.Owner, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) > 0 {
			if len(items) != 1 || items[0].Offer != nil || items[0].Kind != "skill" || items[0].Measure != nil {
				t.Fatal(items)
			}
			break
		}
		if time.Now().After(until) {
			t.Fatal("serve did not propose the skill")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.shutdown()
}

func TestSkillCLIMeasurePendingCleanup(t *testing.T) {
	k, _, id := proposeCLISkill(t)
	t.Setenv("ADA_CLI_LOCK_WORKTREE", "1")
	spec := filepath.Join(k.dir, "eval.json")
	if err := os.WriteFile(spec, []byte(`{"skills":true,"repeats":2,"matrix":[{"backend":"claude"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, _, err := k.invoke(t, "measure", id, "-spec", spec, "-held-out", "0.5", "-seed", "9")
	if out != "better: more passing gates\n" || err == nil || !strings.Contains(err.Error(), "cleanup needs retry") {
		t.Fatalf("measure output %q, error %v", out, err)
	}
	_, _, err = k.invoke(t, "keep", id, "-outside")
	if err == nil || !strings.Contains(err.Error(), "cleanup from an earlier measurement is pending and will be retried on the next command") {
		t.Fatalf("keep error %v", err)
	}
}

func TestSkillCLIFailedRemeasureHasNoVerdict(t *testing.T) {
	k, _, id := proposeCLISkill(t)
	spec := filepath.Join(k.dir, "eval.json")
	if err := os.WriteFile(spec, []byte(`{"skills":true,"repeats":2,"matrix":[{"backend":"claude"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.invoke(t, "measure", id, "-spec", spec, "-held-out", "0.5", "-seed", "9"); err != nil {
		t.Fatal(err)
	}
	out, _, err := k.invoke(t, "measure", id, "-spec", spec, "-held-out", "0.5", "-seed", "10")
	if out != "" || err == nil {
		t.Fatalf("failed remeasurement output %q, error %v", out, err)
	}
}

func crashedSkillMeasurement(t *testing.T) (*learningChat, string) {
	t.Helper()
	c := newLearningChat(t, false)
	repo := cliSkillRepo(t)
	c.settings.WorkspaceRoots = []string{repo}
	c.settings.HRN.Path = os.Args[0]
	c.settings.Learning.Skills = true
	store := learn.Store{Memory: c.app.Store, Roots: []string{repo}}
	if err := store.RecordDelegation(firstRun, "another", "abc"); err != nil {
		t.Fatal(err)
	}
	items, err := store.ProposeSkills(t.Context(), c.model, c.settings.Owner, c.settings.SkillReview())
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	dir := filepath.Join(c.dir, "learning", "skills")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	intent := fmt.Sprintf(`{"work":"w-ABCDEFGH","base":%q,"started":%q}`, strings.TrimSpace(string(base)), time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, items[0].ID+".intent"), []byte(intent), 0600); err != nil {
		t.Fatal(err)
	}
	c.settings.WorkspaceRoots = []string{t.TempDir()}
	return c, items[0].ID
}

func TestServeReportsMeasureRecovery(t *testing.T) {
	c, id := crashedSkillMeasurement(t)
	lines := make(logLines, 32)
	c.startLearning(lines)
	lines.wait(t, "Stopped measurement recovery for "+id)
	c.shutdown()
}

func TestServeReportsMeasureRecoveryBeforeSweep(t *testing.T) {
	c, id := crashedSkillMeasurement(t)
	c.settings.Learning.ProposeEvery = 0
	if err := c.app.Store.Link("echo", "alice", c.settings.Owner); err != nil {
		t.Fatal(err)
	}
	lines := make(logLines, 32)
	c.startLearning(lines)
	c.inbound("check-offer", "alice", "keep", false)
	c.ack("check-offer")
	lines.wait(t, "Stopped measurement recovery for "+id)
	c.shutdown()
}

func TestSkillListEscapesEachLine(t *testing.T) {
	p := learn.Proposal{Kind: "skill", SourceRuns: []string{firstRun}, Skill: &learn.Skill{Name: "a\x1b", Description: "b\r", Body: "check\x1b[2J\nagain"}}
	var out strings.Builder
	if err := printProposals(&out, []learn.Proposal{p}, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{terminalField(p.Skill.Name), terminalField(p.Skill.Description), "\n    check\\x1b[2J\n    again\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q missing %q", out.String(), want)
		}
	}
}

func TestSkillListRecoversAbandonedMeasure(t *testing.T) {
	for _, stage := range []string{"moved-repo", "outside-roots", "changed-head", "missing-gitfile", "moved-tip"} {
		t.Run(stage, func(t *testing.T) {
			k, repo, id := proposeCLISkill(t)
			git := func(args ...string) string {
				t.Helper()
				out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			base := git("rev-parse", "HEAD")
			git("-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "--allow-empty", "-qm", "owner change")
			tip := git("rev-parse", "HEAD")
			branch := "ada/skill-" + strings.ToLower(id)
			git("branch", branch, tip)
			commit := base
			skills := filepath.Join(k.dir, "learning", "skills")
			if err := os.MkdirAll(skills, 0700); err != nil {
				t.Fatal(err)
			}
			const work = "w-ABCDEFGH"
			dir := filepath.Join(skills, work)
			switch stage {
			case "moved-repo":
				moved := filepath.Join(t.TempDir(), "moved")
				if err := os.Rename(repo, moved); err != nil {
					t.Fatal(err)
				}
				repo = moved
			case "outside-roots":
				k.settings["workspace_roots"] = []string{t.TempDir()}
			case "changed-head":
				commit = tip
				git("worktree", "add", "--detach", dir, tip)
			case "missing-gitfile":
				commit = tip
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			journal := filepath.Join(skills, id+".intent")
			data, err := json.Marshal(map[string]any{"work": work, "base": base, "commit": commit, "started": time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journal, data, 0600); err != nil {
				t.Fatal(err)
			}
			out, diag, err := k.invoke(t, "list")
			if err != nil || !strings.Contains(out, id) || !strings.Contains(diag, id) || git("rev-parse", branch) != tip {
				t.Fatal(out, diag, err)
			}
			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}
