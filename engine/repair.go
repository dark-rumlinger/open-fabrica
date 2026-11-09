package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"openfabrica/internal/stringutil"
	"openfabrica/providers/llm"
	"openfabrica/store"
)

// The repair loop. After the build, the validators and the explore probe test what
// was built; before this loop their findings only reached the owner as a count.
// Now each one becomes an open checklist item and a short build segment fixes them,
// reseeded on the same cached prefix as the build with a repair instruction instead
// of the build one. Unfinished plan items ride along, so a build that stopped early
// gets one more go at them. Bounded in rounds and turns; whatever is left ships as
// known issues, exactly as before.

const (
	// maxRepairRounds bounds how often the build is reopened. Each round catches what
	// the previous fix revealed; the loop also stops early once a round no longer
	// reduces the findings (see repairProgress).
	maxRepairRounds = 4
	// repairMaxIterations bounds one repair segment's turns, far below a build's.
	repairMaxIterations = 40
	// maxRepairItems caps the checklist items one round adds, so one noisy category
	// cannot make a round as long as the build. Findings sharing a page and category
	// are one item, so this is a cap on distinct problems.
	maxRepairItems = 20
	// maxRepairFindings caps the findings read into those items.
	maxRepairFindings = 60
	// TaskOriginRepair marks checklist items the repair loop added.
	TaskOriginRepair = "repair"
)

// repairFindings picks the findings worth a repair turn: deterministic errors only
// (FixableErrors). A red user flow is included: it names the step that failed and the
// reason, and shipping a known-red journey whose cause the message states is worse than
// one more round. The repair instruction says how to tell an app bug from a script step.
func repairFindings(issues []Issue) []Issue {
	out := FixableErrors(issues)
	// What a user meets first takes the round's slots: a page that throws or shows
	// nothing, then missing behaviour, then controls that misbehave, then the rest.
	sort.SliceStable(out, func(i, j int) bool { return repairRank(out[i].Category) < repairRank(out[j].Category) })
	if len(out) > maxRepairFindings {
		out = out[:maxRepairFindings]
	}
	return out
}

// repairRanks orders finding categories by how directly a user meets them; an
// unlisted category ranks after every listed one.
var repairRanks = map[string]int{
	"runtime": 0, "blank_page": 0, "inert_page": 0, "hidden_ui": 0, "js_syntax": 0, "module_imports": 0,
	specGapCategory: 1,
	"flow":          2,
	"click_error":   2, "inert_click": 2, "dead_control": 2, "dead_toggle": 2, "covered_control": 2,
	"layout_overflow": 3, "dead_link": 3, "style_drift": 3,
}

func repairRank(category string) int {
	if r, ok := repairRanks[category]; ok {
		return r
	}
	return len(repairRanks)
}

// findingWhere names where a finding is: its page, else its file, else the project.
func findingWhere(is Issue) string {
	switch {
	case is.Page != "":
		return is.Page
	case is.File != "":
		return is.File
	}
	return "the project"
}

// repairTaskID is the checklist id of the item that answers a finding in a round.
func repairTaskID(is Issue, round int) string {
	return fmt.Sprintf("fix-%d-%s", round, stringutil.TaskSlug(is.Category+" "+findingWhere(is)))
}

// repairTasks turns findings into checklist items. The finding's own message is the
// intent: it names the page, the control or file, and the fix. Findings of one category
// on one page are one item (seven controls covered by one overlay are one problem), so
// the round's slots go to distinct problems and the builder sees the whole picture.
func repairTasks(findings []Issue, round int) []Task {
	tasks := make([]Task, 0, len(findings))
	index := map[string]int{}
	for _, is := range findings {
		where := findingWhere(is)
		key := is.Category + "\x00" + where
		if i, ok := index[key]; ok {
			tasks[i].Intent += "\n\n" + strings.TrimSpace(is.Message)
			continue
		}
		index[key] = len(tasks)
		tasks = append(tasks, Task{
			ID:       repairTaskID(is, round),
			Title:    fmt.Sprintf("Fix %s on %s", repairLabel(is.Category), where),
			Intent:   strings.TrimSpace(is.Message),
			DoneWhen: "The problem described no longer happens.",
			Origin:   TaskOriginRepair,
		})
	}
	if len(tasks) > maxRepairItems {
		tasks = tasks[:maxRepairItems]
	}
	return tasks
}

// repairSignatures records, per repair item, the signatures of the findings it answers,
// so a dispute of the item can be matched to the findings of the next sweep.
func (w *Worker) repairSignatures(findings []Issue, round int) {
	if w.repairSigs == nil {
		w.repairSigs = map[string][]string{}
	}
	for _, is := range findings {
		if is.Signature == "" {
			continue
		}
		id := repairTaskID(is, round)
		w.repairSigs[id] = append(w.repairSigs[id], is.Category+"\x00"+is.Signature)
	}
}

// disputableCategories are the explore findings a builder may complete as wrong: they
// are heuristics about what a click visibly did, never a thrown error or missing code.
var disputableCategories = map[string]bool{"inert_click": true, "dead_control": true, "dead_toggle": true, "covered_control": true}

// disputePrefix starts the result of a repair item whose finding is wrong.
const disputePrefix = "disputed:"

// collectDisputes reads the repair items the builder completed as "disputed: <why>" and
// remembers their findings so the next sweep does not report them again.
func (w *Worker) collectDisputes() {
	rows, err := w.pdb.Reader().Query(`SELECT id, COALESCE(result,'') FROM of_tasks WHERE origin = ? AND status = ?`, TaskOriginRepair, TaskDone)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, result string
		if rows.Scan(&id, &result) != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(result)), disputePrefix) {
			continue
		}
		for _, cs := range w.repairSigs[id] {
			cat, sig, _ := strings.Cut(cs, "\x00")
			if !disputableCategories[cat] {
				continue
			}
			if w.disputed == nil {
				w.disputed = map[string]bool{}
			}
			w.disputed[cat+"\x00"+sig] = true
			w.logger.Info("build: finding disputed by the builder", "item", id, "category", cat, "why", strings.TrimSpace(result))
		}
	}
	if err := rows.Err(); err != nil {
		w.logger.Debug("build: dispute scan ended early", "err", err)
	}
}

// specGapResponses maps each checklist id a spec gap was raised on to what the
// builder answered when it completed the repair item for it, the latest round
// winning. The re-audit shows it beside the item: an auditor that cannot see why
// the builder judged a gap closed (or wrong) would raise the same gap again.
func (w *Worker) specGapResponses() map[string]string {
	rows, err := w.pdb.Reader().Query(`SELECT id, COALESCE(result,'') FROM of_tasks WHERE origin = ? AND status = ? ORDER BY rowid`, TaskOriginRepair, TaskDone)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, result string
		if rows.Scan(&id, &result) != nil || strings.TrimSpace(result) == "" {
			continue
		}
		for _, cs := range w.repairSigs[id] {
			cat, sig, _ := strings.Cut(cs, "\x00")
			if cat == specGapCategory {
				out[specGapTaskID(sig)] = strings.TrimSpace(result)
			}
		}
	}
	if err := rows.Err(); err != nil {
		w.logger.Debug("build: spec-gap response scan ended early", "err", err)
	}
	return out
}

// dropDisputed removes the findings the builder disputed from a sweep.
func (w *Worker) dropDisputed(issues []Issue) []Issue {
	if len(w.disputed) == 0 {
		return issues
	}
	out := issues[:0:0]
	for _, is := range issues {
		if w.disputed[is.Category+"\x00"+is.Signature] {
			continue
		}
		out = append(out, is)
	}
	return out
}

// repairProgress reports whether a round changed what is wrong: it resolved a finding,
// or its own edit introduced one (a regression the next round should fix while the
// cause is fresh). Comparing counts read a round that fixed one thing and broke
// another as a stall, and shipped the regression. Only a round that left exactly the
// same findings is not worth another; maxRepairRounds bounds the rest.
func repairProgress(before, after []Issue) bool {
	// Uncapped: repairFindings truncates, which would hide findings on either side.
	was, now := repairKeys(FixableErrors(before)), repairKeys(FixableErrors(after))
	for k := range was {
		if !now[k] {
			return true
		}
	}
	for k := range now {
		if !was[k] {
			return true
		}
	}
	return false
}

// repairKeys identifies findings across sweeps. A signature is built to be stable
// (a flow's is its name, step and failure kind), while the page can move between
// sweeps (a replay lands on a freshly minted /f/<slug>), so a signed finding is keyed
// by category and signature alone.
func repairKeys(issues []Issue) map[string]bool {
	out := make(map[string]bool, len(issues))
	for _, is := range issues {
		if is.Signature != "" {
			out[is.Category+"\x00"+is.Signature] = true
		} else {
			out[is.Fingerprint()] = true
		}
	}
	return out
}

// repairLabel names a finding category in a checklist title the owner also sees.
func repairLabel(category string) string {
	switch category {
	case "inert_click", "dead_control", "inert_page", "dead_toggle":
		return "a control that does nothing"
	case "click_error":
		return "an error when clicking"
	case "covered_control":
		return "a control that cannot be tapped"
	case "layout_overflow":
		return "the phone layout"
	case "style_drift":
		return "inconsistent page styling"
	case "blank_page":
		return "a blank page"
	case "runtime":
		return "a page error"
	case "dead_link":
		return "a broken link"
	case specGapCategory:
		return "something the plan asked for"
	default:
		return strings.ReplaceAll(category, "_", " ")
	}
}

// repairInstruction is the repair segment's task message: what happened, what to do,
// and the checklist with the new items open. User flows are not repaired here - they are
// a separate post-build gate (runFlowGate) - so this is non-flow findings only.
func repairInstruction(d *sql.DB) string {
	var b strings.Builder
	b.WriteString("[mode: repair]\n\n")
	b.WriteString("The build is finished, and the platform has tested it against the plan (where the project has pages: every page loaded in a real browser, each control clicked, the layout checked at desktop and phone sizes). ")
	b.WriteString("What it found is on your checklist as open `fix-` items, each naming the problem and where it is; plan items left unchecked are open too.\n\n")
	b.WriteString("For each open item: read the code involved first, fix the cause with the smallest change that does it, ")
	b.WriteString("verify it with `manage_testing action=explore page_path=...` for a click finding or `test_page` for a page error, then complete it. ")
	b.WriteString("Change nothing else: no rewrite of working code (a finding about inconsistent styling is the exception: fix it in the shared stylesheet). ")
	b.WriteString("Never add decorative animation or fake feedback to make a control look active, and never turn off pointer events on a modal or intro to make a covered control clickable. ")
	b.WriteString("A click finding that is wrong (the control really works, but through audio, a canvas or 3D scene, or a native dialog the check cannot see) ")
	b.WriteString("is completed with a result that starts with `disputed:` and says why in one line; any other finding that is wrong is completed with a one-line result saying why (the re-check reads it). ")
	b.WriteString("Then stop.\n\n")
	b.WriteString(renderChecklist(d))
	return b.String()
}

// repairSpec is the build spec with the repair instruction as its task and a short
// turn budget. owner, shared and intent are the build's own, so the provider cache
// the build warmed serves the repair's prefix.
func (w *Worker) repairSpec(plan *Plan) SessionSpec {
	spec := w.buildWholeSpec(plan, "")
	spec.MaxIterations = repairMaxIterations
	seedFn := func() ([]llm.Message, error) {
		return seed{
			owner:  w.ownerContext() + w.ownerMemoryBlock(),
			shared: w.buildSharedSeed(plan),
			stable: w.buildIntentSeed(plan),
			task:   repairInstruction(w.pdb.Reader()),
		}.messages(), nil
	}
	spec.Seed, _ = seedFn()
	spec.Reseed = seedFn
	return spec
}

// repairRound adds the findings to the checklist and runs one repair segment.
// Reports false when there was nothing to repair.
func (w *Worker) repairRound(ctx context.Context, plan *Plan, findings []Issue, round int) (bool, error) {
	open := openPlanTasks(w.pdb.Reader())
	if len(findings) == 0 && open == 0 {
		return false, nil
	}
	runID := ""
	if w.run != nil {
		runID = w.run.ID
	}
	// Items an earlier round (or an interrupted one) left open are re-derived from this
	// sweep's findings; keeping both would list the same problem twice.
	w.dropOpenRepairTasks()
	if _, err := AddTasks(w.pdb, runID, repairTasks(findings, round)); err != nil {
		return false, fmt.Errorf("adding repair items: %w", err)
	}
	w.repairSignatures(findings, round)
	if round == 1 {
		// Tells the owner why the build is not live yet.
		w.publishMessage("I tested every page and found a few things to fix. Fixing them now.")
	}
	w.publishTaskQueue()
	w.logger.Info("build: repair round", "round", round, "findings", len(findings), "open_items", open)

	sess := w.newStageSession(StageBuild)
	res, err := w.runSpec(ctx, sess, w.repairSpec(plan))
	if w.run != nil {
		w.runlog.editStats(w.run.ID, round, res)
	}
	w.collectDisputes()
	if err == nil && res.Paused {
		if res.EndDetail != endOwnerStop.detail {
			w.pauseAwaitingAnswers()
		}
		return true, errStagePaused
	}
	return true, err
}

// openPlanTasks counts plan items still to build. Blocked items are left out (the
// build treats them as resolved), and so are earlier repair items: whether those
// still matter is the next sweep's call, not the checklist's.
func openPlanTasks(d *sql.DB) int {
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM of_tasks WHERE status IN (?, ?) AND COALESCE(origin, '') <> ?`,
		TaskPending, TaskActive, TaskOriginRepair).Scan(&n)
	return n
}

// dropOpenRepairTasks removes repair items still open after the last round. The
// findings behind them are in the final sweep, which reports them as known issues;
// left on the checklist they would also read as unbuilt plan work.
func (w *Worker) dropOpenRepairTasks() {
	if _, err := w.pdb.ExecWrite(`DELETE FROM of_tasks WHERE origin = ? AND status NOT IN (?, ?)`,
		TaskOriginRepair, TaskDone, TaskBlocked); err != nil {
		w.logger.Warn("build: dropping open repair items failed", "error", err)
	}
}

// styleDriftCategory is the finding the style-drift validator reports under.
const styleDriftCategory = "style_drift"

// styleDriftAsWarning lowers style-drift findings to warnings, leaving the rest as they are.
func styleDriftAsWarning(issues []Issue) []Issue {
	out := make([]Issue, len(issues))
	copy(out, issues)
	for i := range out {
		if out[i].Category == styleDriftCategory {
			out[i].Severity = store.SeverityWarning
			out[i].Deterministic = false
		}
	}
	return out
}
