package main

// Result collection for `openfabrica eval`: everything it reports about one build is
// read straight from that project's database, read-only, so a finished run can be
// re-collected at any time and the numbers mean the same thing the engine meant.

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// evalResult is what one brief's build produced. Acceptance is the headline: error
// findings a user would hit (the explore click-through, unmet spec items) plus every
// planned item left unbuilt.
type evalResult struct {
	Brief       string  `json:"brief"`
	ProjectID   int     `json:"project_id"`
	RunID       string  `json:"run_id"`
	Status      string  `json:"status"`
	Stage       string  `json:"stage"`
	PauseReason string  `json:"pause_reason,omitempty"`
	DurationSec float64 `json:"duration_sec"`
	Error       string  `json:"error,omitempty"`

	Tasks        int `json:"tasks"`
	Unfinished   int `json:"unfinished"`
	RepairsDone  int `json:"repairs_done"`
	Features     int `json:"features"`
	FeaturesOut  int `json:"features_excluded"`
	FeaturesLost int `json:"features_uncovered"`

	SpecMet      int     `json:"spec_met"`
	SpecPartial  int     `json:"spec_partial"`
	SpecMissing  int     `json:"spec_missing"`
	SpecMetRatio float64 `json:"spec_met_ratio"`

	Errors     map[string]int `json:"errors,omitempty"`   // final sweep, error findings by category
	Warnings   map[string]int `json:"warnings,omitempty"` // final sweep, warnings by category
	Acceptance int            `json:"acceptance_failures"`

	Calls        int `json:"calls"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CacheRead    int `json:"cache_read_tokens"`
	BilledInput  int `json:"billed_input_tokens"`
}

// acceptanceCategories are the final-sweep findings a user meets directly: what the
// explore probe clicked and saw fail, and what the spec audit found missing.
var acceptanceCategories = map[string]bool{
	"inert_click": true, "click_error": true, "covered_control": true,
	"layout_overflow": true, "blank_page": true, "spec_gap": true,
	"runtime": true, "dead_control": true, "inert_page": true, "hidden_ui": true, "style_drift": true,
	"flow": true, "websocket": true, "js_syntax": true, "module_imports": true, "undefined_reference": true,
}

// collectResult reads the latest run of a project database into r. It never writes.
func collectResult(pdb *sql.DB, r *evalResult) error {
	var planJSON, started, completed sql.NullString
	err := pdb.QueryRow(`SELECT id, status, stage, COALESCE(pause_reason,''), COALESCE(plan_json,''),
			started_at, completed_at
		FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1`).
		Scan(&r.RunID, &r.Status, &r.Stage, &r.PauseReason, &planJSON, &started, &completed)
	if err != nil {
		return err
	}
	if s, ok := parseSQLTime(started.String); ok {
		if c, ok := parseSQLTime(completed.String); ok {
			r.DurationSec = c.Sub(s).Seconds()
		}
	}

	collectTasks(pdb, r)
	collectFeatures(planJSON.String, r)
	collectFinalSweep(pdb, r)
	// Work the checklist never finished counts whether or not a final sweep was recorded:
	// a build that died before its sweep has no validation row and would otherwise score
	// as flawless.
	r.Acceptance += r.Unfinished
	// A run that did not complete shipped nothing a user could meet.
	if r.Status != "completed" {
		r.Acceptance++
	}
	collectSpecAudit(pdb, r)
	collectCost(pdb, r)
	return nil
}

func collectTasks(pdb *sql.DB, r *evalResult) {
	rows, err := pdb.Query(`SELECT status, COALESCE(origin,'') FROM of_tasks`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status, origin string
		if rows.Scan(&status, &origin) != nil {
			continue
		}
		if origin == "repair" {
			if status == "done" {
				r.RepairsDone++
			}
			continue
		}
		r.Tasks++
		if status != "done" {
			r.Unfinished++
		}
	}
	_ = rows.Err()
}

// collectFeatures counts the brief's features and how the plan accounted for them.
func collectFeatures(planJSON string, r *evalResult) {
	if planJSON == "" {
		return
	}
	var plan struct {
		Features []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"features"`
		Tasks []struct {
			Covers []string `json:"covers"`
		} `json:"tasks"`
		Exclusions json.RawMessage `json:"exclusions"`
	}
	if json.Unmarshal([]byte(planJSON), &plan) != nil {
		return
	}
	covered := map[string]bool{}
	for _, t := range plan.Tasks {
		for _, c := range t.Covers {
			covered[strings.TrimSpace(c)] = true
		}
	}
	var exclusions []string
	_ = json.Unmarshal(plan.Exclusions, &exclusions)
	for _, f := range plan.Features {
		r.Features++
		switch {
		case covered[f.ID]:
		case excludes(exclusions, f.ID, f.Name):
			r.FeaturesOut++
		default:
			r.FeaturesLost++
		}
	}
}

func excludes(exclusions []string, id, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, e := range exclusions {
		l := strings.ToLower(strings.TrimSpace(e))
		if strings.HasPrefix(l, id+":") || strings.HasPrefix(l, id+" ") || (name != "" && strings.Contains(l, name)) {
			return true
		}
	}
	return false
}

// collectFinalSweep reads the authoritative final validator sweep of the run.
func collectFinalSweep(pdb *sql.DB, r *evalResult) {
	var payload string
	if pdb.QueryRow(`SELECT payload FROM of_run_log WHERE run_id = ? AND kind = 'validation' ORDER BY id DESC LIMIT 1`,
		r.RunID).Scan(&payload) != nil {
		return
	}
	var sweep struct {
		Issues []struct {
			Severity string `json:"severity"`
			Category string `json:"category"`
		} `json:"issues"`
	}
	if json.Unmarshal([]byte(payload), &sweep) != nil {
		return
	}
	for _, is := range sweep.Issues {
		if is.Severity == "error" {
			if r.Errors == nil {
				r.Errors = map[string]int{}
			}
			r.Errors[is.Category]++
			if acceptanceCategories[is.Category] {
				r.Acceptance++
			}
		} else {
			if r.Warnings == nil {
				r.Warnings = map[string]int{}
			}
			r.Warnings[is.Category]++
		}
	}
}

// collectSpecAudit merges every spec-audit verdict of the run, the latest per item
// winning: after a repair round only the flagged items are audited again.
func collectSpecAudit(pdb *sql.DB, r *evalResult) {
	rows, err := pdb.Query(`SELECT payload FROM of_run_log WHERE run_id = ? AND kind = 'issue' AND ref = 'spec_audit' ORDER BY id`, r.RunID)
	if err != nil {
		return
	}
	defer rows.Close()
	latest := map[string]string{}
	for rows.Next() {
		var payload string
		if rows.Scan(&payload) != nil {
			continue
		}
		var audit struct {
			Verdicts []struct {
				ID      string `json:"id"`
				Verdict string `json:"verdict"`
			} `json:"verdicts"`
		}
		if json.Unmarshal([]byte(payload), &audit) != nil {
			continue
		}
		for _, v := range audit.Verdicts {
			latest[v.ID] = v.Verdict
		}
	}
	_ = rows.Err()
	for _, v := range latest {
		switch v {
		case "met":
			r.SpecMet++
		case "partial":
			r.SpecPartial++
		case "missing":
			r.SpecMissing++
		}
	}
	if n := r.SpecMet + r.SpecPartial + r.SpecMissing; n > 0 {
		r.SpecMetRatio = float64(r.SpecMet) / float64(n)
	}
}

// collectCost sums the run's LLM calls. Billed input is what the provider charges
// input for: fresh input plus cache reads and writes.
func collectCost(pdb *sql.DB, r *evalResult) {
	var write int
	_ = pdb.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_read_input_tokens),0), COALESCE(SUM(cache_creation_input_tokens),0)
		FROM of_llm_calls WHERE run_id = ?`, r.RunID).
		Scan(&r.Calls, &r.InputTokens, &r.OutputTokens, &r.CacheRead, &write)
	r.BilledInput = r.InputTokens + r.CacheRead + write
}
