package learn

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/adanext-ada/hrnexec"
	"github.com/cristiangirlea/adanext-ada/memory"
)

type MeasureOptions struct {
	Options       hrnexec.Options
	Spec, Base    string
	HeldOut, Seed string
	Timeout       time.Duration
	Diagnostics   io.Writer
}

func validateSkillSpec(data []byte) error {
	type switches struct {
		Backend  string
		Skills   *bool
		Lean     *bool
		NoSkills bool `json:"no_skills"`
		Dir      *string
		MCP      []string
	}
	var spec struct {
		switches
		Tasks   []switches
		Matrix  []switches
		Repeats int
	}
	if _, err := specFields(data); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("read eval spec: %w", err)
	}
	// Ada pins the eval to her worktree, so a spec may not point it elsewhere.
	if spec.Dir != nil || slices.ContainsFunc(append(spec.Tasks, spec.Matrix...), func(s switches) bool { return s.Dir != nil }) {
		return fmt.Errorf("eval spec must not set dir: Ada runs it in the measurement worktree")
	}
	// hrn joins a relative mcp path to dir, which Ada moves to the worktree.
	for _, s := range append(append(spec.Tasks, spec.Matrix...), spec.switches) {
		if slices.ContainsFunc(s.MCP, func(path string) bool { return !filepath.IsAbs(path) }) {
			return fmt.Errorf("eval spec mcp paths must be absolute")
		}
	}
	if len(spec.Matrix) != 1 {
		return fmt.Errorf("spec matrix must have exactly one variant")
	}
	v := spec.Matrix[0]
	backend := v.Backend
	if backend == "" {
		backend = spec.Backend
	}
	if backend != "claude" {
		return fmt.Errorf("eval variant requires the claude backend")
	}
	if v.NoSkills || spec.NoSkills {
		return fmt.Errorf("eval spec sets no_skills")
	}
	loadsSkills := func(task switches) bool {
		skills := cmp.Or(v.Skills, task.Skills, spec.Skills)
		lean := cmp.Or(v.Lean, task.Lean, spec.Lean)
		return skills != nil && *skills || lean != nil && !*lean
	}
	if !loadsSkills(switches{}) {
		return fmt.Errorf("eval spec must enable skills or set lean to false")
	}
	for _, task := range spec.Tasks {
		if v.Backend == "" && task.Backend != "" && task.Backend != "claude" {
			return fmt.Errorf("eval task overrides the claude backend")
		}
		if !loadsSkills(task) {
			return fmt.Errorf("eval task disables skills")
		}
		if task.NoSkills {
			return fmt.Errorf("eval task sets no_skills")
		}
	}
	if spec.Repeats < 2 {
		return fmt.Errorf("eval spec requires at least two repeats")
	}
	return nil
}

// hrn runs a spec in the spec file's folder unless it names a dir, so Ada passes a copy pinned to her worktree.
func pinnedSpecPath(dir string) string { return dir + ".spec.json" }

// The copy sorts its keys, so a repeated or case-variant key could change which value hrn reads.
func specFields(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("read eval spec: expected a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("read eval spec: %w", err)
		}
		key := tok.(string)
		for seen := range fields {
			if strings.EqualFold(seen, key) {
				return nil, fmt.Errorf("eval spec repeats the key %q", key)
			}
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, fmt.Errorf("read eval spec: %w", err)
		}
		fields[key] = raw
	}
	if _, err := d.Token(); err != nil {
		return nil, fmt.Errorf("read eval spec: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("read eval spec: data after the object")
	}
	return fields, nil
}

func writePinnedSpec(data []byte, dir string) (string, error) {
	fields, err := specFields(data)
	if err != nil {
		return "", err
	}
	pinned, err := json.Marshal(dir)
	if err != nil {
		return "", err
	}
	fields["dir"] = pinned
	out, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	path := pinnedSpecPath(dir)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// Each measurement has its own journal so an owner can decide while hrn runs.
type measureIntent struct {
	Admin   string       `json:"admin,omitempty"`
	Work    string       `json:"work"`
	Base    string       `json:"base"`
	Commit  string       `json:"commit,omitempty"`
	Started time.Time    `json:"started"`
	Outcome *Measurement `json:"outcome,omitempty"`
}

var workName = regexp.MustCompile(`^w-[A-Z2-7]{8}$`)

func measureJournal(id string) string { return filepath.Join("skills", id+".intent") }

func skillsDir(root *os.Root) (string, error) {
	info, err := root.Lstat("skills")
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("skills must be a local directory")
	}
	abs, err := filepath.Abs(filepath.Join(root.Name(), "skills"))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func (tx *transaction) recoverMeasures() error {
	_, err := skillsDir(tx.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range tx.Proposals {
		p := &tx.Proposals[i]
		if p.Kind != "skill" {
			continue
		}
		_, err := tx.root.Lstat(measureJournal(p.ID))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		unlock, err := memory.TryMeasureLock(tx.root, p.ID)
		if errors.Is(err, memory.ErrMeasuring) {
			continue
		}
		if err != nil {
			return err
		}
		data, err := tx.root.ReadFile(measureJournal(p.ID))
		if err == nil {
			err = tx.recoverMeasure(p, data)
		} else {
			err = fmt.Errorf("%w: %w", errUnsafeMeasure, err)
		}
		var note string
		switch {
		case errors.Is(err, errUnsafeMeasure):
			note = fmt.Sprintf("Stopped measurement recovery for %s: %v", p.ID, err)
			err = tx.root.Remove(measureJournal(p.ID))
		case errors.Is(err, errRetryMeasure):
			note = fmt.Sprintf("Deferred measurement cleanup for %s: %v", p.ID, err)
			err = nil
		}
		if note != "" {
			tx.note = note
			if tx.store.Diagnose != nil {
				tx.store.Diagnose(note)
			}
		}
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

var errUnsafeMeasure = errors.New("cannot safely replay measure journal")
var errRetryMeasure = errors.New("measurement cleanup needs retry")

func (tx *transaction) recoverMeasure(p *Proposal, data []byte) error {
	if _, err := objectWithOptional(data, []string{"commit", "outcome", "admin"}, "work", "base", "started"); err != nil {
		return fmt.Errorf("%w: %w", errUnsafeMeasure, err)
	}
	var op measureIntent
	if err := json.Unmarshal(data, &op); err != nil {
		return fmt.Errorf("%w: %w", errUnsafeMeasure, err)
	}
	if !workName.MatchString(op.Work) || !gitHash.MatchString(op.Base) || op.Started.Before(p.Created) || op.Commit != "" && !gitHash.MatchString(op.Commit) {
		return fmt.Errorf("%w: invalid intent", errUnsafeMeasure)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := op.validateOutcome(p); err != nil {
		return err
	}
	keep := op.Outcome != nil && p.Status == "pending" && op.Outcome.Verdict == "better"
	if op.Outcome != nil && p.Status == "pending" {
		if err := tx.recordMeasurement(ctx, p, op.Outcome); err != nil {
			return err
		}
	}
	if err := tx.cleanMeasure(ctx, p, &op, keep); err != nil {
		return measureCleanupError(ctx, err)
	}
	return tx.root.Remove(measureJournal(p.ID))
}

func measureCleanupError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", errRetryMeasure, ctx.Err())
	}
	if errors.Is(err, errRetryMeasure) {
		return err
	}
	return fmt.Errorf("%w: %w", errUnsafeMeasure, err)
}

func (op *measureIntent) validateOutcome(p *Proposal) error {
	m := op.Outcome
	if m != nil {
		candidate := *p
		candidate.Measure, candidate.Status, candidate.Decided = m, "pending", time.Time{}
		if m.Commit != op.Commit || m.Base != op.Base || m.At.Before(op.Started) {
			return fmt.Errorf("%w: outcome conflicts with intent", errUnsafeMeasure)
		}
		if err := candidate.validateSkill(); err != nil {
			return fmt.Errorf("%w: %w", errUnsafeMeasure, err)
		}
	}
	return nil
}

func (tx *transaction) recordMeasurement(ctx context.Context, p *Proposal, m *Measurement) error {
	// The saved outcome is the durable marker if cleanup or journal removal needs another load.
	if p.Measure != nil && *p.Measure == *m {
		return nil
	}
	if m.Verdict == "better" {
		if err := tx.store.checkRepo(ctx, *p); err != nil {
			return measureCleanupError(ctx, err)
		}
		tip, err := branchTip(ctx, *p)
		if err != nil {
			return measureCleanupError(ctx, err)
		}
		if tip != m.Commit {
			return fmt.Errorf("%w: branch changed outside Ada", errUnsafeMeasure)
		}
	}
	p.Measure = m
	return tx.save()
}

// A finished eval is journaled before removing either the worktree or the losing branch.
func (tx *transaction) applyMeasurement(clean, request context.Context, p *Proposal, op *measureIntent) error {
	if err := op.validateOutcome(p); err != nil {
		return err
	}
	m := op.Outcome
	record := m != nil && p.Status == "pending" && request.Err() == nil
	keep := record && m.Verdict == "better"
	if err := tx.cleanMeasure(clean, p, op, keep); err != nil {
		return measureCleanupError(clean, err)
	}
	if err := request.Err(); err != nil {
		if m != nil {
			op.Outcome = nil
			if e := memory.WriteJSON(tx.root, measureJournal(p.ID), op); e != nil {
				return e
			}
		}
		if keep {
			if _, e := removeBranch(clean, *p, op.Commit); e != nil {
				return e
			}
		}
		return errors.Join(err, tx.root.Remove(measureJournal(p.ID)))
	}
	if record {
		if err := tx.recordMeasurement(clean, p, m); err != nil {
			return err
		}
	}
	return tx.root.Remove(measureJournal(p.ID))
}

func (tx *transaction) cleanMeasure(ctx context.Context, p *Proposal, op *measureIntent, keep bool) error {
	parent, err := skillsDir(tx.root)
	if err != nil {
		return err
	}
	dir := filepath.Join(parent, op.Work)
	// The copy is Ada's own file, so it goes even when the repository is gone.
	if err := os.Remove(pinnedSpecPath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", errRetryMeasure, err)
	}
	if err := tx.store.checkRepo(ctx, *p); err != nil {
		return err
	}
	tip, err := branchTip(ctx, *p)
	if err != nil {
		return err
	}
	if op.Commit == "" && tip != "" {
		// The unique reflog identity also covers a crash before worktree registration.
		log, err := git(ctx, p.Repo, "reflog", "show", "-1", "--format=%H%x00%ge%x00%gs", "refs/heads/"+p.branch())
		if err != nil {
			return err
		}
		entry := strings.Split(strings.TrimRight(string(log), "\r\n"), "\x00")
		if len(entry) == 3 && entry[0] == tip && ((tip == op.Base && entry[1] == op.Work+"@localhost") || (entry[1] == "ada@localhost" && strings.HasPrefix(entry[2], op.Work+": "))) {
			op.Commit = tip
		}
	}
	if tip != "" && tip != op.Commit {
		return fmt.Errorf("branch changed outside Ada")
	}
	info, statErr := os.Lstat(dir)
	if statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("measurement worktree path changed outside Ada")
		}
		link, err := os.Lstat(filepath.Join(dir, ".git"))
		if err != nil || !link.Mode().IsRegular() {
			return fmt.Errorf("measurement worktree has no regular .git file")
		}
		ref, err := git(ctx, dir, "symbolic-ref", "HEAD")
		if err != nil || strings.TrimSpace(string(ref)) != "refs/heads/"+p.branch() {
			return fmt.Errorf("measurement worktree branch changed outside Ada")
		}
		top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
		if err != nil || !samePath(dir, filepath.FromSlash(strings.TrimRight(string(top), "\r\n"))) {
			return fmt.Errorf("measurement worktree moved outside Ada")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if _, err := git(ctx, p.Repo, "worktree", "remove", "--force", "--", dir); err != nil {
		switch {
		case statErr == nil:
			return fmt.Errorf("%w: %w", errRetryMeasure, err)
		case op.Admin != "":
			if err := removeMeasureAdmin(ctx, p.Repo, dir, op.Admin); err != nil {
				return err
			}
		default:
			// A crash before registration leaves no worktree for Git to remove.
			list, listErr := git(ctx, p.Repo, "worktree", "list", "--porcelain", "-z")
			if listErr != nil {
				return fmt.Errorf("%w: %w", errRetryMeasure, listErr)
			}
			for field := range strings.SplitSeq(string(list), "\x00") {
				if path, ok := strings.CutPrefix(field, "worktree "); ok && samePath(filepath.FromSlash(path), dir) {
					return fmt.Errorf("%w: %w", errRetryMeasure, err)
				}
			}
		}
	}
	if !keep && op.Commit != "" {
		removed, err := removeBranch(ctx, *p, op.Commit)
		if err != nil {
			return err
		}
		if !removed {
			tip, err := branchTip(ctx, *p)
			if err != nil {
				return err
			}
			if tip != "" {
				return fmt.Errorf("left branch %s because its tip changed or it is checked out", p.branch())
			}
		}
	}
	return nil
}

func (s Store) MeasureSkill(ctx context.Context, user, id string, opts MeasureOptions) (result Proposal, err error) {
	if opts.Spec == "" {
		return result, fmt.Errorf("measure requires -spec FILE")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 4 * time.Hour
	}
	if opts.Timeout < 0 {
		return result, fmt.Errorf("measure timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	var p Proposal
	var unlock func()
	var parent string
	var journalDir *os.Root
	err = s.withUser(user, func(tx *transaction) error {
		item, err := tx.find(id)
		if err != nil {
			return err
		}
		if item.Kind != "skill" || item.Status != "pending" {
			return fmt.Errorf("measure requires a pending skill")
		}
		p = *item
		if err := tx.root.MkdirAll("skills", 0700); err != nil {
			return err
		}
		parent, err = skillsDir(tx.root)
		if err != nil {
			return err
		}
		unlock, err = memory.TryMeasureLock(tx.root, id)
		if err != nil {
			return err
		}
		if _, err := tx.root.Lstat(measureJournal(id)); !errors.Is(err, os.ErrNotExist) {
			unlock()
			if err != nil {
				return err
			}
			return fmt.Errorf("previous measurement cleanup is pending")
		}
		journalDir, err = tx.root.OpenRoot("skills")
		if err != nil {
			unlock()
		}
		return err
	})
	if err != nil {
		return result, err
	}
	defer unlock()
	defer journalDir.Close()
	if err := s.checkRepo(ctx, p); err != nil {
		return result, err
	}
	spec, err := filepath.Abs(opts.Spec)
	if err != nil {
		return result, err
	}
	data, err := os.ReadFile(spec)
	if err != nil {
		return result, err
	}
	if err := validateSkillSpec(data); err != nil {
		return result, err
	}
	if (opts.HeldOut == "") != (opts.Seed == "") {
		return result, fmt.Errorf("specify -held-out and -seed together")
	}
	var split []string
	if opts.HeldOut != "" {
		share, e := strconv.ParseFloat(opts.HeldOut, 64)
		if e != nil || math.IsNaN(share) || share < 0 || share > 1 {
			return result, fmt.Errorf("invalid held-out share")
		}
		if _, e := strconv.ParseUint(opts.Seed, 10, 64); e != nil {
			return result, fmt.Errorf("invalid held-out seed")
		}
		split = []string{"--held-out", opts.HeldOut, "--seed", opts.Seed}
	}
	ref := opts.Base
	if ref == "" {
		ref = "HEAD"
	}
	base, err := revision(ctx, p.Repo, ref)
	if err != nil {
		return result, err
	}
	exists, err := hasTreePath(ctx, p.Repo, base, p.Skill.path())
	if err != nil {
		return result, err
	}
	if exists {
		return result, fmt.Errorf("skill already exists at base")
	}
	tip, err := branchTip(ctx, p)
	if err != nil {
		return result, err
	}
	if tip != "" && (p.Measure == nil || tip != p.Measure.Commit) {
		return result, fmt.Errorf("branch changed outside Ada")
	}
	op := measureIntent{Work: "w-" + rand.Text()[:8], Base: base, Started: time.Now().UTC()}
	dir := filepath.Join(parent, op.Work)
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("worktree path unavailable")
	}
	journal := func() error { return memory.WriteJSON(journalDir, id+".intent", op) }
	err = s.withUser(user, func(tx *transaction) error {
		item, err := tx.find(id)
		if err != nil {
			return err
		}
		if item.Status != "pending" {
			return fmt.Errorf("skill was decided before measurement")
		}
		return journal()
	})
	if err != nil {
		return result, err
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		pending := true
		finishErr := s.withUser(user, func(tx *transaction) error {
			item, e := tx.find(id)
			if e != nil {
				return e
			}
			pending = item.Status == "pending"
			if e := tx.applyMeasurement(clean, ctx, item, &op); e != nil {
				return e
			}
			if err == nil {
				if !pending {
					return fmt.Errorf("skill was decided during measurement")
				}
				result = *item
			}
			return nil
		})
		if finishErr != nil && pending && op.Outcome != nil && ctx.Err() == nil {
			result = p
			result.Measure = op.Outcome
		}
		err = errors.Join(err, finishErr)
	}()
	if tip != "" {
		removed, err := removeBranch(ctx, p, tip)
		if err != nil {
			return result, err
		}
		if !removed {
			return result, fmt.Errorf("branch changed outside Ada")
		}
	}
	if _, err := git(ctx, p.Repo, "-c", "core.logAllRefUpdates=always", "-c", "user.name=Ada", "-c", "user.email="+op.Work+"@localhost", "worktree", "add", "-b", p.branch(), "--", dir, base); err != nil {
		return result, err
	}
	admin, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return result, err
	}
	op.Admin = filepath.Clean(filepath.FromSlash(strings.TrimRight(string(admin), "\r\n")))
	if err := journal(); err != nil {
		return result, err
	}
	if opts.Diagnostics != nil {
		if _, err := fmt.Fprintf(opts.Diagnostics, "%sthis skill will be loaded by the eval's claude runs\n", p.Skill.render()); err != nil {
			return result, err
		}
	}
	if err := writeSkill(dir, *p.Skill); err != nil {
		return result, err
	}
	path := p.Skill.path() + "/SKILL.md"
	if _, err := git(ctx, dir, "add", "--", path); err != nil {
		return result, err
	}
	if _, err := gitWithEnv(ctx, dir, []string{"GIT_REFLOG_ACTION=" + op.Work}, "-c", "core.logAllRefUpdates=always", "-c", "user.name=Ada", "-c", "user.email=ada@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath="+filepath.Join(dir, filepath.FromSlash(path), "no-hooks"), "commit", "--no-verify", "-q", "-m", "Add the "+p.Skill.Name+" skill", "--", path); err != nil {
		return result, err
	}
	entry, err := git(ctx, dir, "reflog", "show", "-1", "--format=%H%x00%gs", "refs/heads/"+p.branch())
	if err != nil {
		return result, err
	}
	commit, action, ok := strings.Cut(strings.TrimRight(string(entry), "\r\n"), "\x00")
	if !ok || !gitHash.MatchString(commit) || !strings.HasPrefix(action, op.Work+": ") {
		return result, fmt.Errorf("branch changed outside Ada")
	}
	op.Commit = commit
	if err := journal(); err != nil {
		return result, err
	}
	if err := verifyBlob(ctx, p, op.Commit); err != nil {
		return result, err
	}
	evalSpec, err := writePinnedSpec(data, dir)
	if err != nil {
		return result, err
	}
	defer os.Remove(evalSpec)
	report, err := hrnexec.EvalSetup(ctx, opts.Options, dir, base, p.Skill.path(), evalSpec, split, opts.Diagnostics)
	if err != nil {
		return result, err
	}
	if report.Head != op.Commit {
		return result, fmt.Errorf("hrn evaluated a different head")
	}
	current, err := os.ReadFile(spec)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(data, current) {
		return result, fmt.Errorf("eval spec changed during measurement")
	}
	op.Outcome = &Measurement{Verdict: report.Verdict, Reason: report.Reason, Base: base, Commit: op.Commit, Branch: p.branch(), Eval: report.Candidate.Eval, BaselineEval: report.Baseline.Eval, SpecSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), At: time.Now().UTC()}
	if err := journal(); err != nil {
		op.Outcome = nil
		return result, err
	}
	return result, nil
}
