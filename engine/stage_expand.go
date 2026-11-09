package engine

// The EXPAND stage: an optional pre-PLAN pass that rewrites a sparse owner
// brief into a fuller, stack-correct one. Gated by projects.ambition_level
// (off = the run starts at PLAN; see Worker.resolveRun). Output is plain
// prose persisted on the run (of_runs.expanded_brief); PLAN prefers it over
// the raw description. One LLM call, no tools, no snapshot.

import (
	"context"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/msgcat"
	"openfabrica/pack"
)

type expandStage struct{}

func (expandStage) Name() string { return StageExpand }

// Info: EXPAND is a single wide-sampling call; 5m is ample headroom for prose emission.
func (expandStage) Info() StageInfo {
	return StageInfo{
		Timeout: 5 * time.Minute,
		StartLines: []string{
			msgcat.Mark("Getting to know your idea. Working out exactly what to build."),
			msgcat.Mark("Let me make sure I really understand what you want first."),
			msgcat.Mark("Reading through your idea and pinning down the details."),
		},
	}
}

func (expandStage) Run(ctx context.Context, w *Worker, r *Run) (Outcome, error) {
	// Self-skips (defense in depth alongside the entry-stage selection):
	// change requests edit an existing plan, a crash-recovery re-entry must
	// not re-expand, and an empty brief has nothing to expand.
	if r.Kind == RunKindUpdate {
		return Outcome{Advance: true}, nil
	}
	if r.ExpandedBrief != "" {
		return Outcome{Advance: true}, nil
	}
	brief := w.projectBrief()
	if brief == "" {
		return Outcome{Advance: true}, nil
	}

	spec := DefaultSpec(StageExpand)
	spec.System = pack.Assemble(pack.AssembleInput{
		Stage:     StageExpand,
		Wave:      pack.Wave,
		Selection: pack.Select(w.effectivePacks(), StageExpand),
		Sentinels: map[string]string{
			"AMBITION_DIRECTIVE": ambitionDirective(w.projectAmbitionLevel()),
		},
	}).System
	spec.Seed = seed{
		owner: w.ownerContext(),
		task:  "Owner's raw brief:\n\n" + brief + "\n\nRewrite it as described above. Output prose only.",
	}.messages()

	sess := w.newStageSession(StageExpand)
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		return Outcome{}, err
	}

	expanded := strings.TrimSpace(res.Content)
	if expanded == "" {
		w.logger.Warn("EXPAND returned empty content, falling through to PLAN with the raw brief")
		return Outcome{Advance: true}, nil
	}
	if err := r.setExpandedBrief(w.pdb, expanded); err != nil {
		w.logger.Warn("persist expanded brief failed, proceeding anyway", "error", err)
	}

	// Emit the brief as a single marked message. The leading marker line is the
	// contract the admin chat and the bots key on to render this as a card
	// with a heading in the reader's language instead of a wall of prose. The
	// raw EXPAND turn is suppressed by the session's assistant hook, so this
	// is the only copy.
	w.publishVerbatim(ExpandedBriefMarker + "\n\n" + expanded)
	return Outcome{Advance: true}, nil
}

// ExpandedBriefMarker is the first line of the message EXPAND publishes. It is a
// contract with the readers of that message (web/admin/app/ui/chat/message.js
// matches the same bytes), which strip it and show a heading in their own
// language, so it stays English whatever the project's reply language is.
const ExpandedBriefMarker = "**Expanded brief**"

// projectAmbitionLevel returns the project's normalised ambition level.
// Falls back to the default so a lookup hiccup never breaks a stage.
func (w *Worker) projectAmbitionLevel() string {
	if project, err := models.GetProjectByID(w.deps.DB.DB, w.projectID); err == nil && project != nil {
		return NormalizeAmbition(project.AmbitionLevel)
	}
	return DefaultAmbition
}

// setExpandedBrief persists EXPAND's output on the run.
func (r *Run) setExpandedBrief(pdb *db.ProjectDB, brief string) error {
	r.ExpandedBrief = brief
	return r.update(pdb, "expanded_brief = ?", brief)
}

// expandDirectives is the {{AMBITION_DIRECTIVE}} paragraph per level. Only
// low/medium/high are represented: a fresh run at "off" enters at PLAN, and the
// off level falls back to low (ambitionDirective) for the resume path where
// ambition changed mid-run.
var expandDirectives = map[string]string{
	AmbitionLow:    "**Ambition: Low.** Keep the expansion lean and close to the original (very roughly 150-350 words - a size signal, not a target). Add only obviously missing pieces; do not invent new directions or features the owner did not imply. Cover the core features and the main happy path; skip secondary actions, edge cases, and elaborate state handling. This overrides any broader-ambition guidance above (the fuller feature set, the depth pass per feature) - only the defining-capability floor still holds.",
	AmbitionMedium: "**Ambition: Medium.** Flesh the brief out into a balanced, realistic product (very roughly 300-600 words - a size signal, not a target). Cover the features the brief and project type genuinely imply to feel finished - as many as it earns, no quota - and describe each one's behaviour rather than listing bare names, giving the core ones their key states (empty, loading, error, success). Stay grounded in what the brief implies; don't gold-plate.",
	AmbitionHigh:   "**Ambition: High.** Describe a fully realised, finished product. Aim very roughly for 500-1000 words - a floor to work from, not a ceiling: when fully spelling out the defining capability's cooperating subsystems (the depth floor above) needs more room, take it. A depth-concentrated app (editor, spreadsheet, modeller, simulation, IDE) whose subsystems each deserve a sentence will and should run longer; never trade away core depth to stay under the word count. Cover the full set of features and screens the project type earns - a finished product of this kind often spans many cooperating views, so name them all - and carry each to real behaviour: secondary actions, edit/delete/undo paths, every in-between state, and what happens at the edges (no results, first-time use, large amounts of data). Spend depth before breadth - a few features carried to their full behaviour beat a longer list of bare names. For a game, that means several mechanics, progression or scoring, and win/lose feedback, not a single interaction.",
}

// ambitionDirective returns the {{AMBITION_DIRECTIVE}} paragraph for a level.
// Unknown levels fall back to the default. "off" shares low's lean intent (the
// same alignment AmbitionTaskBudget applies): if EXPAND does run at off - a
// resumed run whose ambition was lowered mid-flight - the expansion must stay
// close to the original brief.
func ambitionDirective(level string) string {
	level = NormalizeAmbition(level)
	if level == AmbitionOff {
		level = AmbitionLow
	}
	return expandDirectives[level]
}
