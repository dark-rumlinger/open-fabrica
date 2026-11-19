package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"openfabrica/providers/llm"
)

func prePlanResearchGateSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Name:   "pre_plan_research_gate",
		Strict: false,
		Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["needs_research"],
  "properties": {
    "needs_research": {"type": "boolean"}
  }
}`),
	}
}

func parsePrePlanResearchDecision(raw string) bool {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	var out struct {
		NeedsResearch bool `json:"needs_research"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return false
	}
	return out.NeedsResearch
}

// needsPrePlanResearch asks the model in a tiny, zero-tool structured call
// whether the brief asks to research online, inspect an external site, or
// gather current real-world info before PLAN builds the task checklist.
func (w *Worker) needsPrePlanResearch(ctx context.Context, rawBrief, expandedBrief string) bool {
	if w.deps == nil || w.deps.ToolRegistry == nil || w.deps.DB == nil {
		return false
	}
	briefText := strings.TrimSpace(rawBrief)
	if exp := strings.TrimSpace(expandedBrief); exp != "" && exp != briefText {
		briefText += "\n\nExpanded brief:\n" + exp
	}
	if briefText == "" {
		return false
	}

	spec := SessionSpec{
		Stage:          StagePlan,
		MaxIterations:  1,
		MaxTokens:      128,
		Temperature:    f64(0),
		Timeout:        15 * time.Second,
		Thinking:       &thinkingOff,
		Policy:         policyPlan,
		ResponseSchema: prePlanResearchGateSchema(),
		System: `Decide whether this project brief needs live web search or fetching an external webpage before planning.
Return true if the brief asks to find/look up/research information online, build something based on an existing website or domain, or needs current real-world facts (news, reviews, latest docs).
Return false for self-contained apps, games, or tools that do not need live external lookup.
Reply with JSON only: {"needs_research":true} or {"needs_research":false}.`,
		Seed: seed{
			task: briefText,
		}.messages(),
	}

	sess := w.newStageSession(StagePlan)
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("pre-plan research gate failed; skipping research", "error", err)
		}
		return false
	}
	return parsePrePlanResearchDecision(res.Content)
}

// runPrePlanResearch runs a short tool-enabled lookup (manage_web_search +
// manage_web_fetch) before PLAN generates the task checklist, so search
// findings inform the plan's design, libraries, and tasks. Non-fatal on error.
func (w *Worker) runPrePlanResearch(ctx context.Context, rawBrief, expandedBrief string) string {
	if w.deps == nil || w.deps.ToolRegistry == nil || w.deps.DB == nil {
		return ""
	}
	const researchMaxIters = 6
	spec := SessionSpec{
		Stage:         StagePlan,
		MaxIterations: researchMaxIters,
		MaxTokens:     4096,
		Temperature:   f64(0.2),
		Timeout:       2 * time.Minute,
		PreloadTools:  []string{"manage_web_search", "manage_web_fetch"},
		Thinking:      &thinkingOff,
		Policy:        policyPlan,
		OnTurnEnd: func(iter int, _ []llm.ToolCall, _ int) (string, *ending) {
			if iter == researchMaxIters-2 {
				return "You have one turn left in this research step. Stop calling search/fetch tools now and write your concise research summary from what you have already gathered.", nil
			}
			return "", nil
		},
		System: `You are a pre-planning research assistant.
The owner's project brief asks to search online, research external info, or build something based on an existing website.
If the brief names a website URL or domain (e.g. cnn.com, example.com), call manage_web_fetch on https://<domain> with extract_design=true (and/or manage_web_search if needed) to inspect its layout, navigation links, feeds, and visual design tokens (colors, fonts).
If manage_web_fetch hits a consent_gate or returns empty content, pivot to manage_web_search (e.g. querying the site's sections, layout, colors, or RSS feeds) and fetch discovered subpages/feeds.
Then write a concise summary of the key findings, observed site structure/sections, visual style (exact hex colors/fonts/header background when found), and source URLs for the planner. Keep section/navigation suggestions in the same language as the owner's brief unless the brief asks for another language.
Do not output plan JSON — output only the research summary.`,
		Seed: seed{
			task: "Project brief:\n\n" + strings.TrimSpace(rawBrief),
		}.messages(),
	}

	sess := w.newStageSession(StagePlan)
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("pre-plan web research failed; continuing to PLAN", "error", err)
		}
		return ""
	}
	if summary := strings.TrimSpace(res.Content); summary != "" {
		return summary
	}
	// If the gather loop exhausted its turns mid-tool-call without emitting a
	// summary, run one tool-less synthesis turn over the gathered transcript so
	// paid search/fetch results are not discarded.
	if res.Ending == EndExhausted && len(res.Messages) > 0 {
		synthSpec := spec
		synthSpec.MaxIterations = 1
		synthSpec.PreloadTools = nil
		synthSpec.OnTurnEnd = nil
		synthSpec.Seed = append(append([]llm.Message(nil), res.Messages...), llm.Message{
			Role:    llm.RoleUser,
			Content: "Search budget reached. Write your concise research summary now from the tool results already in context. Do not output plan JSON.",
		})
		if synthRes, serr := w.runSpec(ctx, w.newStageSession(StagePlan), synthSpec); serr == nil {
			return strings.TrimSpace(synthRes.Content)
		}
	}
	return ""
}
