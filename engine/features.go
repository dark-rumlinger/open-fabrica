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

const featureExtractSystem = `You list the features a project brief asks for, so a plan can be checked for covering every one.

A feature is one user-facing capability: a screen or view, a tool, an interaction loop, a data the user manages, a background behaviour the user relies on. Name it the way the brief does, and give it the specific behaviours the brief describes for it, each a short phrase ("undo the last move", "sold-out items cannot be ordered").

List every feature the brief names, in the brief's order. Do not invent features, and do not list visual style, tone, colours, fonts, or technology. Merge two mentions of the same thing into one feature.

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
// drops empty entries, caps the list, and assigns ids in order.
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
		if name == "" {
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

// minFeaturesFor is how many features a brief of this length should name. A long brief
// that yields one or two features was read badly (a malformed reply with a single entry
// is the usual cause), and a plan checked against that list is checked against almost
// nothing.
func minFeaturesFor(brief string) int {
	n := len(strings.Fields(brief)) / 100
	if n > 6 {
		n = 6
	}
	return n
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
			correction = fmt.Sprintf("\n\nYour previous reply listed only %d feature(s), but this brief describes more. Go through the brief paragraph by paragraph and list every screen, tool, loop, data set and background behaviour it names, each with its behaviours.", len(best))
		}
	}
	w.logger.Info("brief features extracted", "count", len(best))
	return best
}

// extractFeaturesOnce is one feature-extraction call.
func (w *Worker) extractFeaturesOnce(ctx context.Context, brief, correction string) ([]PlanFeature, error) {
	spec := SessionSpec{
		Stage:         StagePlan,
		System:        featureExtractSystem,
		Seed:          []llm.Message{{Role: llm.RoleUser, Content: "Project brief:\n\n" + strings.TrimSpace(brief) + "\n\nList its features." + correction}},
		MaxIterations: 1,
		MaxTokens:     8192,
		Temperature:   f64(0),
		Timeout:       2 * time.Minute,
		Thinking:      &thinkingOff,
		Policy:        policyPlan,
	}
	sess := w.newStageSession(StagePlan)
	if rp, err := sess.resolver.resolve(); err == nil && supportsStructuredOutput(rp) {
		spec.ResponseSchema = featureSchema()
	}
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		w.logger.Warn("feature extraction failed", "error", err)
		return nil, err
	}
	features, err := parseFeatures(res.Content)
	if err != nil {
		w.logger.Warn("feature list unreadable", "error", err)
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
