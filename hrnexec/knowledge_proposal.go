package hrnexec

import (
	"encoding/json"
	"fmt"

	"github.com/cristiangirlea/adanext-ada/internal/jsonobject"
)

type KnowledgeProposal struct {
	Draft    string          `json:"draft"`
	Proposed int             `json:"proposed"`
	Kept     int             `json:"kept"`
	Reviewed []string        `json:"reviewed"`
	Skipped  []RunReason     `json:"skipped"`
	Deferred []RunReason     `json:"deferred"`
	Failed   []ReviewFailure `json:"failed"`
}

type RunReason struct {
	Run    string `json:"run"`
	Reason string `json:"reason"`
}

type ReviewFailure struct {
	Run       string `json:"run"`
	ReviewRun string `json:"review_run"`
	Error     string `json:"error"`
}

func parseKnowledgeProposal(data []byte, runs []string, output string, partial bool) (KnowledgeProposal, error) {
	var report KnowledgeProposal
	fields, err := jsonobject.Fields(data, nil, "draft", "proposed", "kept", "reviewed", "skipped", "deferred", "failed")
	if err != nil {
		return report, fmt.Errorf("decode knowledge proposal: %w", err)
	}
	for _, list := range []string{"skipped", "deferred", "failed"} {
		keys := []string{"run", "reason"}
		if list == "failed" {
			keys = []string{"run", "review_run", "error"}
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(fields[list], &rows); err != nil {
			return report, err
		}
		for _, row := range rows {
			if _, err := jsonobject.Fields(row, nil, keys...); err != nil {
				return report, fmt.Errorf("decode knowledge proposal %s: %w", list, err)
			}
		}
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, err
	}
	if report.Kept < 0 || report.Proposed < report.Kept || (report.Kept > 0) != (report.Draft != "") ||
		(report.Draft != "" && report.Draft != output) || partial != (len(report.Failed) > 0) {
		return report, fmt.Errorf("knowledge proposal counts, draft or exit status disagree")
	}
	// Require exactly one outcome for every requested run before any markers are saved.
	counts := make(map[string]int, len(runs))
	for _, id := range report.Reviewed {
		counts[id]++
	}
	for _, row := range report.Skipped {
		counts[row.Run]++
	}
	for _, row := range report.Deferred {
		counts[row.Run]++
	}
	for _, row := range report.Failed {
		counts[row.Run]++
	}
	if len(counts) != len(runs) {
		return report, fmt.Errorf("knowledge proposal contains unexpected or missing runs")
	}
	for _, id := range runs {
		if counts[id] != 1 {
			return report, fmt.Errorf("knowledge proposal must report run %s exactly once", id)
		}
	}
	return report, nil
}
