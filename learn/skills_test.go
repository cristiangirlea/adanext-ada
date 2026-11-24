package learn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

const skillRun = "20261123-120000-abcdef"
const skillReply = `{"skill":{"name":"run-checks","description":"Test the build","body":" Check first.\r\nThen build. "}}`
const skillSpec = `{"backend":"claude","skills":true,"matrix":[{}],"repeats":2,"tasks":[{"id":"build","prompt":"Build","gate":["go build ./..."]}]}`

func init() {
	mode := os.Getenv("ADA_SKILL_HELPER")
	if mode == "" || len(os.Args) < 2 {
		return
	}
	if os.Getenv("HRN_OPERATOR_TOKEN") != "" || os.Getenv("ADA_SKILL_PROVIDER") != "" {
		os.Exit(4)
	}
	switch os.Args[1] {
	case "show":
		if len(os.Args) != 4 || os.Args[2] != "--json" {
			os.Exit(2)
		}
		if mode == "malformed-show" && os.Args[3] == skillRun {
			fmt.Print("{")
			os.Exit(0)
		}
		if mode == "oversize-show" {
			fmt.Print(strings.Repeat(" ", 32<<20+1))
			os.Exit(0)
		}
		if (mode == "error-show" || mode == "first-show-fails") && os.Args[3] == skillRun {
			fmt.Fprint(os.Stderr, os.Getenv("HRN_TENANT_TOKEN"), strings.Repeat("x", 900))
			os.Exit(1)
		}
		status := "ok"
		if mode == "failed-run" {
			status = "failed"
		}
		repo := os.Getenv("ADA_SKILL_REPO")
		if later := os.Getenv("ADA_SKILL_LATER_REPO"); later != "" && os.Args[3] != skillRun {
			repo = later
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"record": map[string]any{"task": map[string]string{"prompt": strings.Repeat("p", 5000), "dir": repo}, "result": map[string]string{"status": status, "final": strings.Repeat("f", 9000)}, "extra": true}})
	case "eval":
		args := os.Args[2:]
		if len(args) < 7 || !slices.Equal(args[:2], []string{"setup", "--base"}) || !slices.Equal(args[3:6], []string{"--paths", ".claude/skills/run-checks", "--json"}) || !filepath.IsAbs(args[len(args)-1]) {
			os.Exit(3)
		}
		// Like hrn, run in the spec's dir, resolved from the spec file's folder.
		specPath := args[len(args)-1]
		var specDir struct{ Dir string }
		if data, err := os.ReadFile(specPath); err != nil || json.Unmarshal(data, &specDir) != nil {
			os.Exit(8)
		}
		if !filepath.IsAbs(specDir.Dir) {
			specDir.Dir = filepath.Join(filepath.Dir(specPath), specDir.Dir)
		}
		head, err := exec.Command("git", "-C", specDir.Dir, "rev-parse", "HEAD").Output()
		if err != nil {
			fmt.Fprint(os.Stderr, "hrn: git rev-parse HEAD: not a git repository")
			os.Exit(1)
		}
		if mode == "relative-hrn" {
			path, err := os.Executable()
			if err != nil || !samePath(path, os.Getenv("ADA_SKILL_REAL_HRN")) {
				os.Exit(7)
			}
			mode = "better"
		}
		fmt.Fprintln(os.Stderr, "evaluating")
		switch mode {
		case "error":
			os.Exit(1)
		case "garbage":
			fmt.Print("!")
		case "wrong-head":
			fmt.Print(`{"head":"wrong","verdict":"better","reason":"gate","baseline":{"eval":"b"},"candidate":{"eval":"c"}}`)
		case "oversize":
			fmt.Print(strings.Repeat(" ", 1<<20+1))
		default:
			if mode == "wait" {
				stamp := os.Getenv("ADA_SKILL_STAMP")
				if os.WriteFile(stamp, []byte("ready"), 0600) != nil {
					os.Exit(6)
				}
				for {
					if _, err := os.Stat(stamp + ".done"); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				mode = "better"
			}
			fmt.Printf(`{"head":%q,"verdict":%q,"reason":"gate results","baseline":{"eval":"b"},"candidate":{"eval":"c"},"extra":true}`, strings.TrimSpace(string(head)), mode)
		}
	default:
		return
	}
	os.Exit(0)
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}

type skillFixture struct {
	fixture
	repo   string
	review SkillReview
	model  *fake
	opts   MeasureOptions
}

func newSkillFixture(t *testing.T) *skillFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	testGit(t, repo, "init", "-q", "-b", "main")
	testGit(t, repo, "-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "--allow-empty", "-qm", "base")
	t.Setenv("ADA_SKILL_HELPER", "better")
	t.Setenv("ADA_SKILL_REPO", repo)
	t.Setenv("ADA_SKILL_PROVIDER", "filtered")
	t.Setenv("HRN_OPERATOR_TOKEN", "filtered")
	f := setup(t)
	f.Store.Roots = []string{repo}
	opts := hrnexec.Options{HRNPath: os.Args[0], WithoutEnv: []string{"ADA_SKILL_PROVIDER"}}
	s := &skillFixture{fixture: f, repo: repo, model: &fake{reply: skillReply}, review: SkillReview{Options: opts, Timeout: time.Minute}, opts: MeasureOptions{Options: opts, Spec: filepath.Join(f.dir, "spec.json"), Timeout: time.Minute, Diagnostics: io.Discard}}
	if err := os.WriteFile(s.opts.Spec, []byte(skillSpec), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelegation(skillRun, "another", f.session); err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *skillFixture) propose(t *testing.T) Proposal {
	t.Helper()
	items, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	return items[0]
}
func TestSkillReview(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	if p.Kind != "skill" || !p.Outside || p.Status != "pending" || !slices.Equal(p.SourceRuns, []string{skillRun}) || p.Skill.Body != "Check first.\nThen build." || !sameTestDir(p.Repo, f.repo) {
		t.Fatal(p)
	}
	req := f.model.requests[0]
	in := req.Messages[0].Content[0].Text
	if len(req.Tools) != 0 || strings.Count(in, "pp") != 2048 || strings.Count(in, "ff") != 4096 {
		t.Fatal("unexpected material")
	}
	if ok, err := f.BindOffer("owner", p.ID, Offer{"cli", "local", "me", "delivery", time.Now().Add(time.Hour)}); ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := f.RecordDelegation("20261123-130000-abcdef", "another", f.session); err != nil {
		t.Fatal(err)
	}
	items, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review)
	if err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
}
func sameTestDir(a, b string) bool {
	x, e := os.Stat(a)
	y, e2 := os.Stat(b)
	return e == nil && e2 == nil && os.SameFile(x, y)
}
func TestSkillReviewSkips(t *testing.T) {
	for _, tc := range []string{"null", "status", "outside", "nonrepo", "top-outside", "unborn", "claude-file", "skills-file", "parent-changed", "head", "disk"} {
		t.Run(tc, func(t *testing.T) {
			f := newSkillFixture(t)
			var diag []string
			f.review.Diagnose = func(s string) { diag = append(diag, s) }
			switch tc {
			case "null":
				f.model.reply = `{"skill": null}`
			case "status":
				t.Setenv("ADA_SKILL_HELPER", "failed-run")
			case "outside":
				t.Setenv("ADA_SKILL_REPO", t.TempDir())
			case "nonrepo":
				dir := t.TempDir()
				f.Store.Roots = []string{dir}
				t.Setenv("ADA_SKILL_REPO", dir)
			case "unborn":
				testGit(t, f.repo, "switch", "--orphan", "new")
			case "claude-file", "skills-file", "parent-changed":
				path := filepath.Join(f.repo, ".claude")
				if tc == "skills-file" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(path, "skills")
				}
				block := func() {
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if tc == "parent-changed" {
					f.model.onComplete = block
				} else {
					block()
				}
			case "top-outside":
				dir := filepath.Join(f.repo, "sub")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				f.Store.Roots = []string{dir}
				t.Setenv("ADA_SKILL_REPO", dir)
			default:
				dir := filepath.Join(f.repo, ".claude", "skills", "run-checks")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if tc == "head" {
					file := filepath.Join(dir, "existing")
					if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
						t.Fatal(err)
					}
					testGit(t, f.repo, "add", ".")
					testGit(t, f.repo, "-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "-qm", "existing")
					if err := os.RemoveAll(dir); err != nil {
						t.Fatal(err)
					}
				}
			}
			items, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review)
			if err != nil || len(items) != 0 {
				t.Fatal(items, err)
			}
			if slices.Contains([]string{"status", "outside", "nonrepo", "top-outside", "unborn", "claude-file", "skills-file"}, tc) && (len(diag) != 1 || len(f.model.requests) != 0) {
				t.Fatal(diag)
			}
			if err := f.withUser("owner", func(tx *transaction) error {
				if !tx.SkillReviewedRuns[skillRun] {
					return errors.New("unfinished")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestSkillReplyValidation(t *testing.T) {
	for _, tc := range []struct{ field, value string }{{"name", "../x"}, {"name", "UPPER"}, {"name", "ab"}, {"name", strings.Repeat("a", 41)}, {"description", "a\nb"}, {"description", ""}, {"description", strings.Repeat("é", 201)}, {"description", "in\u200bvisible"}, {"body", "line\n---\nline"}, {"body", "\u0001"}, {"body", "\u202e"}, {"body", "  "}, {"body", strings.Repeat("x", 4097)}} {
		row := map[string]string{"name": "abc", "description": "Desc", "body": "Body"}
		row[tc.field] = tc.value
		b, _ := json.Marshal(map[string]any{"skill": row})
		if _, err := parseSkillReply(provider.Response{Content: []provider.Block{{Type: "text", Text: string(b)}}}); err == nil {
			t.Errorf("accepted invalid %s", tc.field)
		}
	}
	for _, raw := range []string{`{"skill":null,"skill":null}`, `{"Skill":null}`, `{"skill":null} {}`, strings.Replace(skillReply, "Check first.", string([]byte{255}), 1)} {
		if _, err := parseSkillReply(provider.Response{Content: []provider.Block{{Type: "text", Text: raw}}}); err == nil {
			t.Error("accepted malformed reply")
		}
	}
}
func TestSkillReplyAcceptsOneJSONFence(t *testing.T) {
	reply := func(text string) provider.Response {
		return provider.Response{Content: []provider.Block{{Type: "text", Text: text}}}
	}
	s, err := parseSkillReply(reply("```json\n" + skillReply + "\n```\n"))
	if err != nil || s == nil || s.Name == "" {
		t.Fatalf("fenced skill = %+v, %v", s, err)
	}
	if s, err := parseSkillReply(reply("```json\r\n{\"skill\": null}\n```")); err != nil || s != nil {
		t.Fatalf("fenced null = %+v, %v", s, err)
	}
	for _, raw := range []string{"```json\n" + skillReply, "```\n" + skillReply + "\n```", "Here:\n```json\n" + skillReply + "\n```", "```json\n" + skillReply + "\n```\nmore"} {
		if _, err := parseSkillReply(reply(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
func TestSkillShowRefusals(t *testing.T) {
	f := newSkillFixture(t)
	t.Setenv("HRN_TENANT_TOKEN", "test-tenant")
	var diagnostic string
	f.review.Diagnose = func(s string) { diagnostic = s }
	for _, mode := range []string{"oversize-show", "error-show", "malformed-show"} {
		t.Setenv("ADA_SKILL_HELPER", mode)
		_, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review)
		if err != nil || diagnostic == "" || strings.Contains(diagnostic, "test-tenant") || len(diagnostic) > 700 {
			t.Fatal(err)
		}
		if mode == "oversize-show" && !strings.Contains(diagnostic, "32 MiB") {
			t.Fatal(err)
		}
	}
	if len(f.model.requests) != 0 {
		t.Fatal("provider received an unreadable run")
	}
}

func TestSkillMeasureRefusesChangedInputs(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	f.Store.Roots = []string{t.TempDir()}
	if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err == nil {
		t.Fatal("accepted a removed root")
	}
	f.Store.Roots = []string{f.repo}
	dir := filepath.Join(f.repo, ".claude", "skills", "run-checks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "existing"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, f.repo, "add", "--", ".claude")
	testGit(t, f.repo, "-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "-qm", "existing")
	if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err == nil || !strings.Contains(err.Error(), "exists at base") {
		t.Fatal(err)
	}
	if testGit(t, f.repo, "branch", "--list", "ada/*") != "" {
		t.Fatal("created a branch for a refused base")
	}
}

func TestSkillReviewFailures(t *testing.T) {
	f := newSkillFixture(t)
	f.model.err = errors.New("unavailable")
	_, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review)
	if !errors.Is(err, f.model.err) {
		t.Fatal(err)
	}
	f.model.err = nil
	f.model.reply = "bad"
	var diag []string
	f.review.Diagnose = func(s string) { diag = append(diag, s) }
	for i := 0; i <= 3; i++ {
		if err := f.withUser("owner", func(tx *transaction) error {
			if tx.SkillReviewedRuns[skillRun] != (i == 3) || tx.SkillReviewFailures[skillRun] != i%3 {
				return fmt.Errorf("wrong attempt %d", i)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, _ = f.ProposeSkills(t.Context(), f.model, "owner", f.review)
	}
	if len(f.model.requests) != 4 || !strings.Contains(strings.Join(diag, " "), "retired") {
		t.Fatal(diag)
	}
}
func TestSkillMeasureAndDecide(t *testing.T) {
	for _, mode := range []string{"better", "worse", "same", "inconclusive", "garbage", "oversize", "error", "wrong-head"} {
		t.Run(mode, func(t *testing.T) {
			f := newSkillFixture(t)
			p := f.propose(t)
			base := testGit(t, f.repo, "rev-parse", "HEAD")
			branch := "ada/skill-" + strings.ToLower(p.ID)
			decide := func(action string, outside bool) error {
				_, err := f.Decide("owner", p.ID, action, outside, time.Now())
				return err
			}
			if decide("keep", true) == nil {
				t.Fatal("unmeasured keep")
			}
			var progress bytes.Buffer
			f.opts.Diagnostics = &progress
			t.Setenv("ADA_SKILL_HELPER", mode)
			p2, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
			if strings.Count(testGit(t, f.repo, "worktree", "list", "--porcelain"), "worktree ") != 1 {
				t.Fatal("worktree survived")
			}
			if !strings.Contains(progress.String(), "evaluating") {
				t.Fatal(progress.String())
			}
			exists := testGit(t, f.repo, "branch", "--list", branch) != ""
			if exists != (mode == "better") {
				t.Fatal("branch", exists)
			}
			if slices.Contains([]string{"garbage", "oversize", "error", "wrong-head"}, mode) {
				if err == nil {
					t.Fatal("accepted invalid result")
				}
				items, e := f.List("owner", false)
				if e != nil || items[0].Measure != nil {
					t.Fatal(items, e)
				}
				return
			}
			if err != nil || p2.Status != "pending" || p2.Measure.Verdict != mode || p2.Measure.Eval != "c" || p2.Measure.BaselineEval != "b" || p2.Measure.Base != base || len(p2.Measure.SpecSHA256) != 64 {
				t.Fatal(p2, err)
			}
			if mode != "better" {
				if decide("keep", true) == nil {
					t.Fatal("kept", mode)
				}
				return
			}
			if testGit(t, f.repo, "rev-list", "--count", base+".."+branch) != "1" || testGit(t, f.repo, "diff", "--name-only", base, branch) != ".claude/skills/run-checks/SKILL.md" {
				t.Fatal("unexpected commit")
			}
			blob := testGit(t, f.repo, "show", branch+":.claude/skills/run-checks/SKILL.md")
			if blob != "---\nname: run-checks\ndescription: \"Test the build\"\n---\n\nCheck first.\nThen build." {
				t.Fatal(blob)
			}
			if decide("keep", false) == nil || decide("keep", true) != nil {
				t.Fatal("outside decision")
			}
			if decide("confirm", false) == nil || decide("restore", false) == nil {
				t.Fatal("accepted fact action")
			}
			changed, e := f.Sweep("owner", time.Now().Add(24*time.Hour), time.Hour, time.Hour)
			if e != nil || len(changed) != 0 {
				t.Fatal(changed, e)
			}
			if testGit(t, f.repo, "symbolic-ref", "--short", "HEAD") != "main" || testGit(t, f.repo, "rev-parse", "HEAD") != base || len(strings.Fields(testGit(t, f.repo, "for-each-ref", "--format=%(refname)", "refs/heads"))) != 2 {
				t.Fatal("owner branches changed")
			}
			if err := decide("rollback", false); err != nil || testGit(t, f.repo, "branch", "--list", branch) != "" {
				t.Fatal(err)
			}
		})
	}
}
func TestMeasureRelativeRepositorySpec(t *testing.T) {
	f := newSkillFixture(t)
	t.Chdir(f.repo)
	if err := os.WriteFile("eval.json", []byte(skillSpec), 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, f.repo, "add", "--", "eval.json")
	testGit(t, f.repo, "-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "-qm", "add eval")
	f.opts.Spec = "eval.json"
	t.Setenv("ADA_SKILL_HELPER", "better")
	p := f.propose(t)
	measured, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
	if err != nil || measured.Measure == nil || measured.Measure.Verdict != "better" {
		t.Fatal(measured, err)
	}
}
func TestMeasurePinsTheEvalToItsWorktree(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	measured, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
	if err != nil || measured.Measure == nil || measured.Measure.Verdict != "better" {
		t.Fatal(measured, err)
	}
	left, err := filepath.Glob(filepath.Join(f.dir, "learning", "skills", "*.spec.json"))
	if err != nil || len(left) != 0 {
		t.Fatal("pinned eval spec left behind", left, err)
	}
	if current, err := os.ReadFile(f.opts.Spec); err != nil || string(current) != skillSpec {
		t.Fatal("owner's spec changed", err)
	}
	for _, spec := range []string{
		strings.Replace(skillSpec, `"repeats":2`, `"repeats":2,"dir":"."`, 1),
		strings.Replace(skillSpec, `"id":"build"`, `"dir":"x","id":"build"`, 1),
		strings.Replace(skillSpec, "[{}]", `[{"dir":"x"}]`, 1),
	} {
		if err := validateSkillSpec([]byte(spec)); err == nil || !strings.Contains(err.Error(), "dir") {
			t.Errorf("accepted %s: %v", spec, err)
		}
	}
}
func TestPinnedSpecKeepsEverythingButDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w-ABCDEFGH")
	spec := `{"backend":"claude","skills":true,"matrix":[{"name":"c","effort":"high"}],"repeats":2,"timeout":"10m","gate":["go vet ./... && go test ./..."],"tasks":[{"id":"build","prompt":"a < b & c","gate":["go build ./..."],"mcp":["` + filepath.ToSlash(filepath.Join(t.TempDir(), "tools.json")) + `"]}],"max_cost_usd":1.50}`
	path, err := writePinnedSpec([]byte(spec), dir)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := json.Unmarshal([]byte(spec), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(copied, &got); err != nil {
		t.Fatal(err)
	}
	want["dir"] = dir
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("copy = %s", copied)
	}
	if _, err := writePinnedSpec([]byte(spec), dir); err == nil {
		t.Fatal("overwrote an existing copy")
	}
	for _, bad := range []string{
		// Each passes a last-key-wins reading but flips once the copy sorts its keys.
		strings.Replace(skillSpec, `"repeats":2`, `"repeats":2,"no_skills":true,"NO_SKILLS":false`, 1),
		strings.Replace(skillSpec, `"repeats":2`, `"repeats":1,"Repeats":2`, 1),
		strings.Replace(skillSpec, `"skills":true`, `"skills":false,"Skills":true`, 1),
		strings.Replace(skillSpec, `"id":"build"`, `"mcp":["tools.json"],"id":"build"`, 1),
		strings.Replace(skillSpec, `"repeats":2`, `"repeats":2,"mcp":["./tools.json"]`, 1),
		strings.Replace(skillSpec, "[{}]", `[{"mcp":["tools.json"]}]`, 1),
	} {
		if err := validateSkillSpec([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
func TestSkillSpecRefusals(t *testing.T) {
	for _, spec := range []string{strings.Replace(skillSpec, "[{}]", "[{},{}]", 1), strings.Replace(skillSpec, "claude", "codex", 1), strings.Replace(skillSpec, "true", "false", 1), strings.Replace(skillSpec, "[{}]", `[{"no_skills":true}]`, 1), strings.Replace(skillSpec, `"repeats":2`, `"repeats":1`, 1)} {
		if err := validateSkillSpec([]byte(spec)); err == nil {
			t.Error(spec)
		}
	}
}
func TestSkillSpecTaskOverrides(t *testing.T) {
	for _, field := range []string{`"backend":"codex"`, `"no_skills":true`, `"lean":true,"skills":false`} {
		raw := strings.Replace(skillSpec, `"id":"build"`, field+`,"id":"build"`, 1)
		if err := validateSkillSpec([]byte(raw)); err == nil {
			t.Fatal("accepted task override", field)
		}
	}
}
func TestMeasuredSkillOwnership(t *testing.T) {
	for _, mode := range []string{"moved", "different-content", "reject", "merged", "repeat"} {
		t.Run(mode, func(t *testing.T) {
			f := newSkillFixture(t)
			p := f.propose(t)
			p, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := p.Measure
			switch mode {
			case "moved":
				testGit(t, f.repo, "branch", "-f", m.Branch, m.Base)
				if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err == nil || !strings.Contains(err.Error(), "branch changed outside Ada") {
					t.Fatal(err)
				}
				if _, err := f.Decide("owner", p.ID, "keep", true, time.Now()); err == nil {
					t.Fatal("moved branch kept")
				}
				_, note, err := f.DecideWithNote("owner", p.ID, "reject", false, time.Now())
				if err != nil || !strings.Contains(note, "Left branch") || testGit(t, f.repo, "rev-parse", m.Branch) != m.Base {
					t.Fatal(note, err)
				}
			case "different-content":
				err := f.withUser("owner", func(tx *transaction) error { tx.Proposals[0].Skill.Body = "different"; return tx.save() })
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Decide("owner", p.ID, "keep", true, time.Now()); err == nil || !strings.Contains(err.Error(), "SKILL.md differs") {
					t.Fatal(err)
				}
			case "reject":
				if _, err := f.Decide("owner", p.ID, "reject", false, time.Now()); err != nil || testGit(t, f.repo, "branch", "--list", m.Branch) != "" {
					t.Fatal(err)
				}
			case "merged":
				if _, err := f.Decide("owner", p.ID, "keep", true, time.Now()); err != nil {
					t.Fatal(err)
				}
				testGit(t, f.repo, "merge", "--ff-only", m.Branch)
				p, note, err := f.DecideWithNote("owner", p.ID, "rollback", false, time.Now())
				if err != nil || p.Status != "rolled_back" || !strings.Contains(note, "git revert "+m.Commit) || testGit(t, f.repo, "rev-parse", m.Branch) != m.Commit {
					t.Fatal(note, err)
				}
			case "repeat":
				t.Setenv("ADA_SKILL_HELPER", "same")
				again, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
				if err != nil || again.Measure.Verdict != "same" || testGit(t, f.repo, "branch", "--list", m.Branch) != "" {
					t.Fatal(again, err)
				}
			}
		})
	}
}
func TestSkillFileRefusesTampering(t *testing.T) {
	for _, mode := range []string{"fact-with-skill", "bad-name", "fact-with-measure", "false-reviewed", "finished-with-failures"} {
		t.Run(mode, func(t *testing.T) {
			f := newSkillFixture(t)
			p := f.propose(t)
			err := f.withUser("owner", func(tx *transaction) error {
				switch mode {
				case "fact-with-skill", "fact-with-measure":
					fact := Proposal{ID: p.ID, Kind: "fact", Text: "a fact", Session: f.session, Messages: []int{0}, Created: p.Created, Status: "pending"}
					tx.Reviewed[f.session] = 1
					if mode == "fact-with-skill" {
						fact.Skill = p.Skill
					} else {
						fact.Measure = &Measurement{}
					}
					tx.Proposals[0] = fact
				case "bad-name":
					tx.Proposals[0].Skill.Name = "../other"
				case "false-reviewed":
					tx.SkillReviewedRuns[skillRun] = false
				case "finished-with-failures":
					tx.SkillReviewFailures = map[string]int{skillRun: 1}
				}
				return memory.WriteJSON(tx.root, tx.name, tx.state)
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.List("owner", true); err == nil {
				t.Error("loaded", mode)
			}
		})
	}
}

func TestRejectSkillAfterRootRemoved(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	p, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.Store.Roots = nil
	decided, note, err := f.DecideWithNote("owner", p.ID, "reject", false, time.Now())
	if err != nil || decided.Status != "rejected" || !strings.Contains(note, "Left branch") || testGit(t, f.repo, "rev-parse", p.Measure.Branch) != p.Measure.Commit {
		t.Fatal(note, err)
	}
}
func TestCheckedOutSkillDecision(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	p, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, f.repo, "switch", p.Measure.Branch)
	p, note, err := f.DecideWithNote("owner", p.ID, "reject", false, time.Now())
	if err != nil || p.Status != "rejected" || !strings.Contains(note, "Left branch") {
		t.Fatal(p, note, err)
	}
	if _, err := f.List("owner", true); err != nil {
		t.Fatal(err)
	}
}
func TestInterruptedSkillMeasurement(t *testing.T) {
	for _, stage := range []string{"remove-before-save", "branch-before-worktree", "completed-better", "locked-worktree", "detached-worktree", "outside-roots"} {
		t.Run(stage, func(t *testing.T) {
			f := newSkillFixture(t)
			p := f.propose(t)
			p, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := *p.Measure
			op := measureIntent{Work: "w-ABCDEFGH", Base: m.Base, Commit: m.Commit, Started: m.At}
			switch stage {
			case "outside-roots":
				f.Store.Roots = nil
				m.Verdict = "same"
				op.Outcome = &m
				if err := os.WriteFile(pinnedSpecPath(filepath.Join(f.dir, "learning", "skills", op.Work)), []byte(skillSpec), 0600); err != nil {
					t.Fatal(err)
				}
			case "remove-before-save":
				m.Verdict = "worse"
				op.Outcome = &m
				testGit(t, f.repo, "branch", "-D", m.Branch)
			case "completed-better", "locked-worktree", "detached-worktree":
				op.Outcome = &m
				dir := filepath.Join(f.dir, "learning", "skills", op.Work)
				if stage == "locked-worktree" {
					testGit(t, f.repo, "worktree", "add", "--lock", dir, m.Branch)
				}
				if stage == "detached-worktree" {
					testGit(t, f.repo, "worktree", "add", "--detach", dir, m.Commit)
				}
				if stage == "completed-better" {
					if err := os.WriteFile(pinnedSpecPath(dir), []byte(skillSpec), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "branch-before-worktree":
				op.Commit = ""
				testGit(t, f.repo, "branch", "-D", m.Branch)
				testGit(t, f.repo, "-c", "user.name=Ada", "-c", "user.email="+op.Work+"@localhost", "branch", m.Branch, m.Base)
			}
			if err := f.withUser("owner", func(tx *transaction) error {
				tx.Proposals[0].Measure = nil
				if err := tx.save(); err != nil {
					return err
				}
				return memory.WriteJSON(tx.root, measureJournal(p.ID), op)
			}); err != nil {
				t.Fatal(err)
			}
			items, err := f.List("owner", false)
			if err != nil || len(items) != 1 {
				t.Fatal(items, err)
			}
			dir := filepath.Join(f.dir, "learning", "skills", op.Work)
			if _, err := os.Stat(pinnedSpecPath(dir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery left the pinned eval spec", err)
			}
			journal := filepath.Join(f.dir, "learning", measureJournal(p.ID))
			if stage == "locked-worktree" {
				name, err := memory.UserFilename("owner")
				if err != nil {
					t.Fatal(err)
				}
				stateFile := filepath.Join(f.dir, "learning", name)
				stamp := time.Unix(1000000000, 0)
				if err := os.Chtimes(stateFile, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if _, err := os.Stat(journal); err != nil {
						t.Fatal("lost cleanup journal", err)
					}
					items, err = f.List("owner", false)
					if err != nil || len(items) != 1 || items[0].Measure == nil || *items[0].Measure != m {
						t.Fatal("lost finished measurement", items, err)
					}
				}
				if info, err := os.Stat(stateFile); err != nil || !info.ModTime().Equal(stamp) {
					t.Fatal("cleanup rewrote recorded measurement", err)
				}
				if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err == nil {
					t.Fatal("started another measurement during cleanup")
				}
				if _, err := os.Stat(journal); err != nil {
					t.Fatal("replaced cleanup journal", err)
				}
				testGit(t, f.repo, "worktree", "unlock", dir)
				if _, err := f.List("owner", false); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("worktree survived", err)
				}
			}
			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("journal survived", err)
			}
			if stage == "branch-before-worktree" {
				if testGit(t, f.repo, "branch", "--list", m.Branch) != "" || items[0].Measure != nil {
					t.Fatal(items)
				}
			} else if items[0].Measure == nil || items[0].Measure.Verdict != m.Verdict {
				t.Fatal("lost finished measurement", items)
			}
		})
	}
}

func TestConcurrentSkillMeasure(t *testing.T) {
	for _, action := range []string{"reject", "cancel", "busy"} {
		t.Run(action, func(t *testing.T) {
			f := newSkillFixture(t)
			p := f.propose(t)
			t.Setenv("ADA_SKILL_HELPER", "wait")
			stamp := filepath.Join(f.dir, "ready")
			t.Setenv("ADA_SKILL_STAMP", stamp)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				result, err := f.MeasureSkill(ctx, "owner", p.ID, f.opts)
				if action == "busy" && (result.Measure == nil || result.Measure.Verdict != "better" || result.Measure.Reason != "gate results") {
					t.Errorf("finished eval missing from measure result: %+v", result.Measure)
				}
				done <- err
			}()
			until := time.Now().Add(20 * time.Second)
			for {
				if _, err := os.Stat(stamp); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatal(err)
				default:
				}
				if time.Now().After(until) {
					t.Fatal("eval did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err == nil || !strings.Contains(err.Error(), "already being measured") {
				t.Fatal(err)
			}
			if action == "busy" {
				var op measureIntent
				err := f.Memory.WithLearningFiles(func(root *os.Root) error {
					if err := os.WriteFile(stamp+".done", nil, 0600); err != nil {
						return err
					}
					if err := <-done; err == nil || !strings.Contains(err.Error(), "learning.lock") {
						t.Fatalf("record error = %v", err)
					}
					data, err := root.ReadFile(measureJournal(p.ID))
					if err != nil {
						return err
					}
					return json.Unmarshal(data, &op)
				})
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(f.dir, "learning", "skills", op.Work)
				admin := testGit(t, dir, "rev-parse", "--git-dir")
				if !samePath(admin, op.Admin) || op.Outcome == nil {
					t.Fatal("incomplete journal", op)
				}
				testGit(t, f.repo, "worktree", "lock", dir)
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				list, err := f.List("owner", true)
				if _, statErr := os.Stat(admin); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("admin entry survived: %v", statErr)
				}
				if err != nil || len(list) != 1 || list[0].Measure == nil {
					t.Fatal("missing completed eval", list, err)
				}
				m := list[0].Measure
				if m.Verdict != "better" || m.Eval != "c" || m.BaselineEval != "b" || m.Reason != "gate results" || testGit(t, f.repo, "rev-parse", m.Branch) != m.Commit {
					t.Fatal(m)
				}
				return
			}
			if action == "cancel" {
				cancel()
			} else {
				if _, err := f.Decide("owner", p.ID, "reject", false, time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stamp+".done", nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := <-done
			if action == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			list, e := f.List("owner", true)
			if e != nil || list[0].Measure != nil || testGit(t, f.repo, "branch", "--list", "ada/*") != "" || strings.Count(testGit(t, f.repo, "worktree", "list", "--porcelain"), "worktree ") != 1 {
				t.Fatal(list, e)
			}
		})
	}
}

func TestSkillReviewContinuesAfterShowFailure(t *testing.T) {
	f := newSkillFixture(t)
	t.Setenv("ADA_SKILL_HELPER", "first-show-fails")
	checkSkillReviewContinues(t, f)
}

func TestSkillReviewContinuesAfterRepositoryReadFailure(t *testing.T) {
	f := newSkillFixture(t)
	laterRepo := filepath.Join(t.TempDir(), "later")
	testGit(t, f.repo, "clone", "-q", "--local", f.repo, laterRepo)
	t.Setenv("ADA_SKILL_LATER_REPO", laterRepo)
	f.Store.Roots = append(f.Store.Roots, laterRepo)
	parent := filepath.Join(f.repo, ".claude")
	skills := filepath.Join(parent, "skills")
	if err := os.MkdirAll(skills, 0700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		acl := func(args ...string) {
			t.Helper()
			if out, err := exec.Command("icacls", append([]string{parent}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("icacls: %v: %s", err, out)
			}
		}
		acl("/deny", "*S-1-1-0:(OI)(CI)(RX)")
		t.Cleanup(func() { acl("/remove:d", "*S-1-1-0") })
		if _, err := os.Lstat(skills); err == nil {
			t.Skip("test user can read past a deny ACL")
		}
	} else {
		if err := os.Chmod(parent, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(parent, 0700) })
		if _, err := os.Lstat(skills); err == nil {
			t.Skip("test user can traverse directories without permission")
		}
	}
	checkSkillReviewContinues(t, f)
}

func checkSkillReviewContinues(t *testing.T, f *skillFixture) {
	t.Helper()
	const later = "20261123-130000-abcdef"
	if err := f.RecordDelegation(later, "owner", f.session); err != nil {
		t.Fatal(err)
	}
	var notes []string
	f.review.Diagnose = func(s string) { notes = append(notes, s) }
	for attempt := 1; attempt <= 3; attempt++ {
		items, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 1 && (len(items) != 1 || items[0].SourceRuns[0] != later) {
			t.Fatal(items)
		}
		if err := f.withUser("owner", func(tx *transaction) error {
			if !tx.SkillReviewedRuns[later] || tx.SkillReviewedRuns[skillRun] != (attempt == 3) || tx.SkillReviewFailures[skillRun] != attempt%3 {
				t.Fatalf("attempt %d: %+v", attempt, tx.state)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(notes) != 3 || !strings.Contains(notes[2], "retired") || len(f.model.requests) != 1 {
		t.Fatal(notes)
	}
}

func TestSkillBatchInterruptionKeepsEarlierReviews(t *testing.T) {
	for _, interrupt := range []string{"provider", "cancel"} {
		t.Run(interrupt, func(t *testing.T) {
			f := newSkillFixture(t)
			runs := []string{skillRun, "20261123-130000-abcdef", "20261123-140000-abcdef"}
			for _, id := range runs[1:] {
				if err := f.RecordDelegation(id, "owner", f.session); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("provider unavailable")
			f.model.onComplete = func() {
				n := len(f.model.requests)
				f.model.reply = strings.Replace(skillReply, "run-checks", fmt.Sprintf("run-checks-%d", n), 1)
				if n == 3 {
					if interrupt == "cancel" {
						want = context.Canceled
						cancel()
					} else {
						f.model.err = want
					}
				}
			}
			if _, err := f.ProposeSkills(ctx, f.model, "owner", f.review); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if err := f.withUser("owner", func(tx *transaction) error {
				if len(tx.SkillReviewFailures) != 0 || len(tx.SkillReviewedRuns) != 2 || len(tx.Proposals) != 2 {
					t.Fatal(tx.state)
				}
				for i, id := range runs[:2] {
					if !tx.SkillReviewedRuns[id] || !slices.Equal(tx.Proposals[i].SourceRuns, []string{id}) {
						t.Fatal(tx.state)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f.model.err = nil
			p := f.propose(t)
			if len(f.model.requests) != 4 || !slices.Equal(p.SourceRuns, runs[2:]) {
				t.Fatal(p.SourceRuns)
			}
		})
	}
}

func TestSkillReviewEnvironmentStopsBatch(t *testing.T) {
	for _, cause := range []string{"hrn", "git", "configured root", "deadline exceeded"} {
		t.Run(cause, func(t *testing.T) {
			f := newSkillFixture(t)
			if err := f.RecordDelegation("20261123-130000-abcdef", "owner", f.session); err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "hrn":
				f.review.Options.HRNPath = filepath.Join(t.TempDir(), "missing-hrn")
			case "git":
				t.Setenv("PATH", t.TempDir())
			case "configured root":
				f.Store.Roots = []string{filepath.Join(t.TempDir(), "missing")}
			default:
				f.review.Timeout = time.Nanosecond
			}
			for range 3 {
				var notes []string
				f.review.Diagnose = func(s string) { notes = append(notes, s) }
				_, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review)
				if err != nil {
					notes = append(notes, err.Error())
				}
				if len(notes) != 1 || !strings.Contains(notes[0], cause) {
					t.Errorf("diagnostics = %q", notes)
				}
				if err := f.withUser("owner", func(tx *transaction) error {
					if len(tx.SkillReviewedRuns)+len(tx.SkillReviewFailures)+len(tx.Proposals) != 0 {
						t.Errorf("review changed state: %+v", tx.state)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMeasureRefusesSpecWithoutArtifacts(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	if err := os.WriteFile(f.opts.Spec, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err == nil || !strings.Contains(err.Error(), "matrix") {
		t.Fatal(err)
	}
	if testGit(t, f.repo, "branch", "--list", p.branch()) != "" || strings.Count(testGit(t, f.repo, "worktree", "list", "--porcelain"), "worktree ") != 1 {
		t.Fatal("unexpected git artifacts")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "learning", measureJournal(p.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestMeasureUsesAdaRelativeExecutable(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	t.Chdir(t.TempDir())
	rel := filepath.Join("bin", "hrn"+filepath.Ext(os.Args[0]))
	if err := os.Mkdir("bin", 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rel, binary, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.repo, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, rel), binary, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, f.repo, "add", "bin")
	testGit(t, f.repo, "-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "-qm", "decoy")
	t.Setenv("ADA_SKILL_HELPER", "relative-hrn")
	absolute, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADA_SKILL_REAL_HRN", absolute)
	f.opts.Options.HRNPath = rel
	if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionAfterMeasurementTime(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	p, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Decide("owner", p.ID, "reject", false, p.Created); err != nil {
		t.Fatal(err)
	}
	items, err := f.List("owner", true)
	if err != nil || len(items) != 1 || !items[0].Decided.Equal(p.Measure.At) {
		t.Fatal(items, err)
	}
}

func TestTransactionRefusesInvalidSkill(t *testing.T) {
	f := newSkillFixture(t)
	f.propose(t)
	if err := f.withUser("owner", func(tx *transaction) error {
		tx.Proposals[0].Skill.Name = ""
		return tx.save()
	}); err == nil {
		t.Fatal("saved invalid skill")
	}
	if _, err := f.List("owner", true); err != nil {
		t.Fatal(err)
	}
}

func TestMeasurePreservesMissingOwnerWorktree(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	// Git prints the long form of a short (8.3) temp path.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "owner")
	testGit(t, f.repo, "worktree", "add", "--detach", dir, "HEAD")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(testGit(t, f.repo, "worktree", "list", "--porcelain"), filepath.ToSlash(dir)) {
		t.Fatal("pruned owner worktree")
	}
}

func TestRollbackCopiedSkill(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	p, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Decide("owner", p.ID, "keep", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	testGit(t, f.repo, "checkout", p.branch(), "--", p.Skill.path())
	testGit(t, f.repo, "-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "-qm", "install skill")
	_, note, err := f.DecideWithNote("owner", p.ID, "rollback", false, time.Now())
	if err != nil || !strings.Contains(note, p.Skill.path()) || !strings.Contains(note, "git revert "+testGit(t, f.repo, "rev-parse", "HEAD")) || testGit(t, f.repo, "branch", "--list", p.branch()) != "" {
		t.Fatal(note, err)
	}
}

func TestRecoverSkillDecisionAtBranchDelete(t *testing.T) {
	for _, action := range []string{"reject", "rollback"} {
		for _, deleted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deleted=%t", action, deleted), func(t *testing.T) {
				f := newSkillFixture(t)
				p := f.propose(t)
				p, err := f.MeasureSkill(t.Context(), "owner", p.ID, f.opts)
				if err != nil {
					t.Fatal(err)
				}
				if action == "rollback" {
					if _, err := f.Decide("owner", p.ID, "keep", true, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				if deleted {
					testGit(t, f.repo, "branch", "-D", p.branch())
				}
				op := intent{ID: p.ID, Action: action, At: time.Now().UTC()}
				wantTime := op.At
				if action == "reject" && deleted {
					op.At, wantTime = p.Created, p.Measure.At
				}
				if err := f.withUser("owner", func(tx *transaction) error { return memory.WriteJSON(tx.root, tx.journal(), op) }); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					list, err := f.List("owner", true)
					if err != nil || len(list) != 1 || list[0].Status != transitions[action].to || !list[0].Decided.Equal(wantTime) {
						t.Fatal(list, err)
					}
				}
				if testGit(t, f.repo, "branch", "--list", p.branch()) != "" {
					t.Fatal("candidate branch survived")
				}
				name, _ := memory.UserFilename("owner")
				if _, err := os.Stat(filepath.Join(f.dir, "learning", ".intent-"+name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestMeasureRecoveryRefusesUnrelatedAdmin(t *testing.T) {
	f := newSkillFixture(t)
	p := f.propose(t)
	base := testGit(t, f.repo, "rev-parse", "HEAD")
	owner := filepath.Join(t.TempDir(), "owner")
	testGit(t, f.repo, "worktree", "add", "--detach", owner, base)
	admin := filepath.FromSlash(testGit(t, owner, "rev-parse", "--git-dir"))
	for _, path := range []string{admin, t.TempDir()} {
		op := measureIntent{Admin: path, Work: "w-ABCDEFGH", Base: base, Started: time.Now().UTC()}
		if err := f.withUser("owner", func(tx *transaction) error {
			if err := tx.root.MkdirAll("skills", 0700); err != nil {
				return err
			}
			return memory.WriteJSON(tx.root, measureJournal(p.ID), op)
		}); err != nil {
			t.Fatal(err)
		}
		var note string
		f.Store.Diagnose = func(s string) { note = s }
		if _, err := f.List("owner", true); err != nil || note == "" {
			t.Fatal(note, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		if testGit(t, owner, "rev-parse", "HEAD") != base {
			t.Fatal("owner worktree changed")
		}
	}
}

func TestSkillReviewCountsGitError(t *testing.T) {
	f := newSkillFixture(t)
	f.model.onComplete = func() {
		if err := os.WriteFile(filepath.Join(f.repo, ".git", "config"), []byte("["), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.ProposeSkills(t.Context(), f.model, "owner", f.review); err != nil {
		t.Fatal(err)
	}
	if err := f.withUser("owner", func(tx *transaction) error {
		if tx.SkillReviewFailures[skillRun] != 1 || tx.SkillReviewedRuns[skillRun] || len(tx.Proposals) != 0 {
			t.Fatal(tx.state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSkillReviewAllowsOtherRepositorySettings(t *testing.T) {
	f := newSkillFixture(t)
	dir := filepath.Join(f.repo, ".claude")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, f.repo, "add", ".claude")
	testGit(t, f.repo, "-c", "user.name=Test", "-c", "user.email=t@localhost", "commit", "-qm", "settings")
	f.propose(t)
}
