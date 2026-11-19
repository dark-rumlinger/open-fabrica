package engine

// The brief's features, as data. A brief names what the project should do in prose,
// and nothing used to check that the plan covered all of it: a feature the planner
// dropped was simply never built, and no validator could see the gap because there
// was nothing built to inspect. One small structured call lists the features the
// brief names (with the behaviours it gives each); the planner tags every task with
// the feature ids it builds (`covers`), and a coherence rule sends the plan back
// once when a feature is neither covered nor excluded. BUILD and the spec audit see
// the same list.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/jsonrepair"
	"openfabrica/providers/llm"
)

const (
	// maxFeatures and maxFeatureBehaviours bound the list, so a sprawling brief yields
	// a checklist a planner can map, not a requirements document.
	maxFeatures          = 25
	maxFeatureBehaviours = 6
)

// PlanFeature is one user-facing capability the brief names, with the behaviours it
// gives it. Ids are assigned by the engine ("f1", "f2", ...), never the model.
type PlanFeature struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Behaviours []string `json:"behaviours,omitempty"`
}

const featureExtractSystem = `You list the functional features a project brief asks for, so a build plan can be checked for covering every deliverable.

A feature is one distinct, buildable functional capability that maps to a checklist item: a screen, page, or view; a shared navigation shell; an interactive tool, subsystem, or game loop; a data collection the user manages; or a background automation/webhook. Name it the way the brief does, and give it the specific functional behaviours the brief describes for it, each a short phrase ("undo the last move", "sold-out items cannot be ordered").

List every functional feature the brief names, in the brief's order. Merge two mentions of the same capability into one feature, and fold sample/seed content or a screen's empty/error/mobile behaviour into the feature that displays it rather than listing it as its own feature.

Do not invent features, and NEVER list cross-cutting quality attributes, motion/polish, or non-functional requirements as features: responsive or mobile layout, page transitions, scroll/reveal or lazy-load animations, keyboard focus states or accessibility, performance/speed, visual style, tone, colours, fonts, or technology. Those apply across the whole site via its design system and layout rather than as separate features.

Reply with JSON only: {"features":[{"name":"...","behaviours":["..."]}]}.`

// featureSchema is the structured reply.
func featureSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Name:   "brief_features",
		Strict: false,
		Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["features"],
  "properties": {
    "features": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "behaviours"],
        "properties": {
          "name": {"type": "string"},
          "behaviours": {"type": "array", "items": {"type": "string"}}
        }
      }
    }
  }
}`),
	}
}

// parseFeatures reads the reply (tolerating fences or prose around the object),
// drops empty or cross-cutting non-functional entries, caps the list, and assigns
// ids in order.
func parseFeatures(content string) ([]PlanFeature, error) {
	s := jsonrepair.Object(content)
	var reply struct {
		Features []PlanFeature `json:"features"`
	}
	if err := json.Unmarshal([]byte(s), &reply); err != nil {
		return nil, fmt.Errorf("feature list is not JSON: %w", err)
	}
	var out []PlanFeature
	for _, f := range reply.Features {
		name := strings.TrimSpace(f.Name)
		if name == "" || isCrossCuttingQualityFeature(name) {
			continue
		}
		var bs []string
		for _, b := range f.Behaviours {
			if b = strings.TrimSpace(b); b != "" && len(bs) < maxFeatureBehaviours {
				bs = append(bs, b)
			}
		}
		out = append(out, PlanFeature{ID: fmt.Sprintf("f%d", len(out)+1), Name: name, Behaviours: bs})
		if len(out) == maxFeatures {
			break
		}
	}
	return out, nil
}

// deliverableFeatureNouns are words in a feature name that identify a concrete screen,
// shell, data resource, interactive subsystem, or background job. A feature whose name
// contains any of these is kept even when it also mentions mobile, accessibility, or
// animation ("Mobile checkout page", "Accessibility settings panel").
var deliverableFeatureNouns = []string{
	"page", "screen", "view", "section", "bar", "nav", "menu", "header", "footer",
	"form", "table", "gallery", "list", "detail", "dashboard", "modal", "drawer", "panel",
	"editor", "canvas", "game", "board", "feed", "api", "endpoint", "webhook", "cron",
	"schedule", "auth", "login", "signup", "account", "admin", "inbox", "cart", "checkout",
	"search", "filter", "chart", "map", "player", "chat", "message", "room", "lobby",
	"settings", "profile", "calculator", "upload", "export", "import", "report",
}

// crossCuttingQualityCues are headings the extractor emits when it mistakes a
// closing polish/UX/design paragraph for a standalone feature ("Responsive and smooth
// experience", "Accessibility focus states", "Reveal and lazy-load animations"). Left in
// the list, they either force the planner to invent a polish task (which STAGE.md forbids)
// or bounce the plan for an uncovered-feature retry.
var crossCuttingQualityCues = []string{
	"responsive", "mobile layout", "mobile experience", "smooth experience",
	"smooth transition", "page transition", "view transition",
	"reveal animation", "scroll reveal", "lazy-load", "lazy load",
	"focus state", "keyboard accessibility", "accessible focus", "wcag", "a11y",
	"visual polish", "micro-interaction", "microinteraction",
	"performance and speed", "fast loading",
}

// isCrossCuttingQualityFeature reports whether a feature name is a pure cross-cutting
// quality/polish/motion heading rather than a functional deliverable. Conservative:
// any concrete deliverable noun in the name keeps the feature.
func isCrossCuttingQualityFeature(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, w := range words {
		for _, noun := range deliverableFeatureNouns {
			if w == noun || w == noun+"s" {
				return false
			}
		}
	}
	for _, cue := range crossCuttingQualityCues {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

// minFeaturesFor is the minimum feature count expected from a multi-paragraph brief.
// A long brief that yields zero or one feature was read badly (a malformed reply with a
// single entry is the usual cause). Capped at 2 so an expanded brief (which spends several
// paragraphs on visual direction, sample content, and polish) is never forced to retry and
// pad a 2- or 3-feature site with non-functional fluff.
func minFeaturesFor(brief string) int {
	if len(strings.Fields(brief)) >= 200 {
		return 2
	}
	return 0
}

// extractFeatures lists the brief's features, asking once more when the first reply is
// unreadable or far too short for the brief. Any failure yields none, which turns the
// coverage check off for this plan rather than blocking it.
func (w *Worker) extractFeatures(ctx context.Context, brief string) []PlanFeature {
	if strings.TrimSpace(brief) == "" {
		return nil
	}
	var best []PlanFeature
	correction := ""
	for attempt := 1; attempt <= 2; attempt++ {
		features, err := w.extractFeaturesOnce(ctx, brief, correction)
		if err == nil && len(features) > len(best) {
			best = features
		}
		if err == nil && len(best) >= minFeaturesFor(brief) {
			break
		}
		if err != nil {
			correction = "\n\nYour previous reply could not be read as JSON. Reply with one valid JSON object only: double quotes, no trailing commas, no commentary."
		} else {
			correction = fmt.Sprintf("\n\nYour previous reply listed only %d feature(s), but this brief describes more functional deliverables. List every distinct screen/view, tool, interaction loop, data collection, and background automation it names (do not include responsive layout, transitions, animations, focus states, or visual style).", len(best))
		}
	}
	w.logger.Info("brief features extracted", "count", len(best))
	return best
}

// extractFeaturesOnce is one feature-extraction call.
func (w *Worker) extractFeaturesOnce(ctx context.Context, brief, correction string) ([]PlanFeature, error) {
	features, _, err := runStructuredCall(ctx, w, structuredCallOpts{
		Stage:       StagePlan,
		System:      featureExtractSystem,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "Project brief:\n\n" + strings.TrimSpace(brief) + "\n\nList its features." + correction}},
		Schema:      featureSchema(),
		MaxTokens:   8192,
		Timeout:     2 * time.Minute,
		Temperature: f64(0),
		Thinking:    &thinkingOff,
	}, parseFeatures)
	if err != nil {
		w.logger.Warn("feature extraction failed", "error", err)
		return nil, err
	}
	return features, nil
}

// renderFeaturesForPlan is the planner's list, with the rule that binds it.
func renderFeaturesForPlan(features []PlanFeature) string {
	if len(features) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Features the brief names (by id). Put the ids a task builds in its `covers`; every id must be covered by a task or excluded with an `exclusions` entry that starts with it (\"f4: <reason>\").\n")
	for _, f := range features {
		writeFeatureLine(&b, f)
		b.WriteByte('\n')
	}
	return b.String()
}

func writeFeatureLine(b *strings.Builder, f PlanFeature) {
	fmt.Fprintf(b, "- `%s` %s", f.ID, f.Name)
	if len(f.Behaviours) > 0 {
		b.WriteString(": " + strings.Join(f.Behaviours, "; "))
	}
}

// featureExclusion returns the exclusion entry naming the feature by id (or by its
// exact name), or "".
func featureExclusion(f PlanFeature, exclusions []string) string {
	name := strings.ToLower(strings.TrimSpace(f.Name))
	for _, e := range exclusions {
		lower := strings.ToLower(strings.TrimSpace(e))
		if strings.HasPrefix(lower, f.ID+":") || strings.HasPrefix(lower, f.ID+" ") || lower == f.ID ||
			(name != "" && strings.Contains(lower, name)) {
			return e
		}
	}
	return ""
}

// featureCoverers maps each feature id to the tasks that cover it.
func featureCoverers(plan *Plan) map[string][]string {
	out := map[string][]string{}
	if plan == nil {
		return out
	}
	for _, t := range plan.Tasks {
		id := t.ID
		if id == "" {
			id = t.Title
		}
		for _, c := range t.Covers {
			c = strings.TrimSpace(c)
			out[c] = append(out[c], id)
		}
	}
	return out
}

// featureCoverageFeedback names the features the plan neither covers nor excludes,
// as a corrective message for the planner. "" when every feature is accounted for.
func featureCoverageFeedback(plan *Plan, features []PlanFeature) string {
	if plan == nil || len(features) == 0 || len(plan.Tasks) == 0 {
		return ""
	}
	covered := featureCoverers(plan)
	var missing []string
	for _, f := range features {
		if len(covered[f.ID]) > 0 || featureExclusion(f, []string(plan.Exclusions)) != "" {
			continue
		}
		var b strings.Builder
		writeFeatureLine(&b, f)
		missing = append(missing, b.String())
	}
	if len(missing) == 0 {
		return ""
	}
	return "These features from the brief are in no task's `covers` and not excluded, so they would never be built:\n" +
		strings.Join(missing, "\n") +
		"\nFor each: add its id to the `covers` of the task that builds it (extend that task's intent and done_when to name the behaviours), add a task when none fits, " +
		"or, when the task budget cannot fit it, add an `exclusions` entry starting with its id (\"f4: <reason>\"). Re-emit the full plan."
}

// renderFeaturesForBuild shows BUILD and the spec audit what the brief asked for and
// where the plan put each feature: the tasks that build it, or the exclusion that
// leaves it out. Excluded features are marked so a brief that still describes them
// is never read as an instruction to build them.
func renderFeaturesForBuild(plan *Plan) string {
	if plan == nil || len(plan.Features) == 0 {
		return ""
	}
	covered := featureCoverers(plan)
	var b strings.Builder
	b.WriteString("\n## Brief features\n")
	b.WriteString("What the brief asks for, by feature, and the checklist items that build each. A feature marked OUT OF SCOPE is not built, even where the brief still describes it.\n")
	for _, f := range plan.Features {
		writeFeatureLine(&b, f)
		switch {
		case len(covered[f.ID]) > 0:
			b.WriteString(" (built by: `" + strings.Join(covered[f.ID], "`, `") + "`)")
		case featureExclusion(f, []string(plan.Exclusions)) != "":
			b.WriteString(" (OUT OF SCOPE: " + featureExclusion(f, []string(plan.Exclusions)) + ")")
		default:
			b.WriteString(" (no item builds this)")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
