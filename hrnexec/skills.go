package hrnexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

var ErrRunRecord = errors.New("unreadable run record")

type RunEvidence struct {
	Task   struct{ Prompt, Dir string }
	Result struct{ Status, Final string }
}

func reviewEnvironment(opts Options) Options {
	opts.WithoutEnv = append(slices.Clone(opts.WithoutEnv), "HRN_OPERATOR_TOKEN")
	return opts
}

func ShowRun(ctx context.Context, opts Options, timeout time.Duration, runID string) (RunEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := runReport(ctx, reviewEnvironment(opts), "", 32<<20, nil, "show", "--json", runID)
	var response struct{ Record RunEvidence }
	if ctx.Err() != nil {
		return RunEvidence{}, ctx.Err()
	}
	if errors.Is(err, errOutputLimit) {
		return RunEvidence{}, fmt.Errorf("%w: %w", ErrRunRecord, err)
	}
	if err != nil {
		return RunEvidence{}, err
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return RunEvidence{}, fmt.Errorf("%w: %w", ErrRunRecord, err)
	}
	if response.Record.Result.Status == "" {
		return RunEvidence{}, fmt.Errorf("%w: missing run result status", ErrRunRecord)
	}
	return response.Record, nil
}

type EvalReport struct {
	Verdict, Reason, Head string
	Baseline, Candidate   struct{ Eval string }
}

func EvalSetup(ctx context.Context, opts Options, dir, base, path, spec string, split []string, progress io.Writer) (EvalReport, error) {
	args := []string{"eval", "setup", "--base", base, "--paths", path, "--json"}
	args = append(args, split...)
	args = append(args, spec)
	data, err := runReport(ctx, reviewEnvironment(opts), dir, 1<<20, progress, args...)
	var report EvalReport
	if err == nil {
		err = json.Unmarshal(data, &report)
	}
	if err == nil && (!slices.Contains([]string{"better", "worse", "same", "inconclusive"}, report.Verdict) || strings.TrimSpace(report.Reason) == "" || report.Head == "" || report.Baseline.Eval == "" || report.Candidate.Eval == "") {
		err = fmt.Errorf("incomplete hrn eval setup report or invalid verdict")
	}
	return report, err
}
