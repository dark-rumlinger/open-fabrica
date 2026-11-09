package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/engine/snapshotbuild"
)

// refuseGoLive tells the owner why the deploy stopped and names every finding that
// stopped it. It never flips the project mode or calls goLive, so "live" keeps meaning
// "a real user can actually use this". Names pages and plain symptoms, not validator
// categories: the owner asked for a working app, not a lint report.
func (w *Worker) refuseGoLive(blocking []Issue) {
	var b strings.Builder
	b.WriteString(w.tr("**I stopped before deploying this.**") + "\n\n")
	if len(blocking) == 1 {
		b.WriteString(w.tr("The build finished, but it has an issue that would leave the app broken for anyone who opened it:") + "\n\n")
	} else {
		b.WriteString(w.tr("The build finished, but it has %d issues that would leave the app broken for anyone who opened it:", len(blocking)) + "\n\n")
	}
	for _, is := range blocking {
		where := is.Page
		if where == "" {
			where = is.File
		}
		if where == "" {
			where = w.tr("the project")
		}
		fmt.Fprintf(&b, "- **%s**: %s\n", where, firstSentence(is.Message))
	}
	b.WriteString("\n" + w.tr("Tell me which to tackle first and I'll get on it."))
	// The fixed lines are translated above; the findings are validator text and
	// stay as they are, so the whole is published as is.
	w.publishVerbatim(b.String())
	w.logger.Warn("build: refused go-live", "blocking", len(blocking),
		"categories", blockingCategorySummary(blocking))
}

// firstSentence trims a validator message down to its opening clause for an owner-facing
// list. Validator prose is written for the model (repro detail, remediation, error
// dumps); the owner needs the symptom, and the detail is one question away in chat.
func firstSentence(msg string) string {
	msg = strings.TrimSpace(msg)
	end := len(msg)
	// A hard line break ends the clause.
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		end = i
	}
	// Cut at the first sentence-ending period (followed by space, newline, or end). A
	// period inside a token (a filename like `app.js`, a decimal) is followed by a
	// non-space, so it does not truncate the clause.
	for i := 0; i < end; i++ {
		if msg[i] == '.' && (i+1 >= len(msg) || msg[i+1] == ' ' || msg[i+1] == '\n') {
			end = i
			break
		}
	}
	return strings.TrimSpace(msg[:end])
}

// unfinishedTasks returns the checklist items that never reached done, in plan
// order. Blocked counts: a blocked task is unbuilt work the owner is owed an
// answer about, not a resolved one.
func unfinishedTasks(d *sql.DB) ([]Task, error) {
	all, err := LoadTasks(d)
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, t := range all {
		// A repair item is the sweep's finding in checklist form; what it left open is
		// already reported as a known issue, not as planned work.
		if t.Status != TaskDone && t.Origin != TaskOriginRepair {
			out = append(out, t)
		}
	}
	return out, nil
}

// secretNotes tells the owner about every secret saved during this run. A build
// that gates a page behind a passcode it stored itself otherwise ships a page the
// owner cannot open. Names only: a value never goes into chat history.
func (w *Worker) secretNotes() []string {
	if w.run == nil {
		return nil
	}
	rows, err := w.pdb.Reader().Query(
		`SELECT name FROM of_secrets
		 WHERE updated_at >= (SELECT started_at FROM of_runs WHERE id = ?)
		 ORDER BY name`, w.run.ID)
	if err != nil {
		w.logger.Warn("go-live: reading this run's secrets failed", "error", err)
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			names = append(names, "`"+name+"`")
		}
	}
	if err := rows.Err(); err != nil {
		w.logger.Warn("go-live: reading this run's secrets ended early", "error", err)
	}
	if len(names) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"The build saved %s under [API keys](/projects/%d/secrets). If you didn't give me the value, I picked it: set your own there before you rely on it.",
		strings.Join(names, ", "), w.projectID)}
}

// completeBuild announces go-live and flips the project mode. UI projects that didn't
// author a favicon get a deterministic platform one, and the go-live message carries the
// "what's inside" overview plus a sign-in block (with an auto-provisioned demo account when
// the project has auth). unfinished checklist items are named in the message so the owner
// learns what is missing from us, not from the running project.
func (w *Worker) completeBuild(ctx context.Context, knownIssues bool, issueReasons []string, unfinished []Task, flowUnverified []string, flows *flowCheck) {
	// One snapshot feeds the favicon guarantee, the "what's inside" overview, and
	// the structured go-live card payload below.
	snap, snapErr := snapshotbuild.BuildProjectSnapshot(ctx, w.pdb.Reader(), nil)
	if snapErr != nil {
		w.logger.Warn("go-live snapshot failed", "error", snapErr)
	}
	if snap != nil {
		// Guarantee a project-matched favicon for UI projects that didn't author one.
		w.ensureFavicon(snap)
	}

	port := w.deps.PublicPort
	if port == 0 {
		port = defaultPublicPort
	}
	previewURL := fmt.Sprintf("http://localhost:%d", port)
	_, domain := w.probeTarget()
	if domain != "" {
		d := strings.ToLower(domain)
		switch {
		case strings.HasSuffix(d, ".localhost"):
			previewURL = fmt.Sprintf("http://%s:%d", domain, port)
		case d == "localhost":
			previewURL = fmt.Sprintf("http://localhost:%d", port)
		default:
			previewURL = "https://" + domain
		}
	}

	header := pickPhrase(
		"**Your project is live!**",
		"**It's live!**",
		"**Done. Your project is live!**",
	)
	tail := "You can chat here anytime to ask questions or request changes."
	if knownIssues {
		header = "**Your project is deployed, with known issues.**"
		tail = "Some issues couldn't be resolved automatically. Tell me what to tackle first in chat and I'll patch it."
	}
	if len(unfinished) > 0 {
		tail = "Tell me to carry on and I'll pick these up, or say which to do first."
	} else if !knownIssues && len(flowUnverified) > 0 {
		tail = flowUnverifiedTail()
	}

	// Provision the demo login once; both the markdown sign-in block and the
	// structured card payload reference this same account.
	var demo *demoLogin
	if snapshotHasAuth(snap) {
		demo, _ = w.provisionDemoLogin()
	}

	settingsURL := fmt.Sprintf("/projects/%d/settings-sharing", w.projectID)
	var b strings.Builder
	fmt.Fprintf(&b,
		"%s\n\n- **Preview locally:** [%s](%s)\n- **Share with the world:** [Open Settings](%s) to connect a domain and manage visibility\n",
		header, previewURL, previewURL, settingsURL)
	notes := w.secretNotes()
	b.WriteString(goLiveSections(snap, demo, previewURL))
	b.WriteString(notesSection(notes))
	b.WriteString(unfinishedSection(unfinished))
	b.WriteString(flowUnverifiedSection(flowUnverified))
	b.WriteString(flowSkippedSection(flows))
	b.WriteString("\n" + tail)
	// Append the structured go-live card payload. The chat UI renders it as a
	// card and falls back to the markdown above if it can't parse it.
	payload := buildGoLivePayload(snap, demo, previewURL, settingsURL, knownIssues, flowUnverified, flows)
	if knownIssues {
		payload.IssueReasons = issueReasons
	}
	payload.Notes = notes
	if marker := goLiveMarker(payload); marker != "" {
		b.WriteString("\n\n" + marker)
	}
	w.publishVerbatim(b.String())

	w.goLive()
}
