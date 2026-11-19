package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/jsonrepair"
	"openfabrica/probe"
	"openfabrica/providers/llm"
	"openfabrica/store"
)

const (
	maxVisualAuditPages  = 4
	maxVisualAudits      = 2
	maxVisualDefects     = 5
	visualDefectCategory = "visual_defect"
)

var captureScreenshotsFn = probe.CapturePageScreenshots

const visualAuditSystem = `You inspect rendered desktop (1280x800) and mobile (390x844) screenshots of a built web app, site, or game.

Report ONLY concrete, objective visual defects visible in the screenshots:
- Overlapping or colliding text, HUD elements, or controls
- Text or UI cut off, clipped, or overflowing off-screen
- Illegible text due to near-identical foreground/background contrast
- Blank, black, or uninitialized canvas/3D viewport where a game or scene should render
- Completely unstyled / broken layout (raw unformatted dump, collapsed columns)

Do NOT report subjective style preferences, minor polish ideas, or speculative issues not visible in the screenshots. Each defect must name the page, viewport, what is visually wrong, and a concrete CSS/HTML/JS fix. Reply with the JSON object only.`

func visualAuditSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Name:   "visual_audit",
		Strict: false,
		Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["items"],
  "properties": {
    "items": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["page", "viewport", "severity", "issue", "fix"],
        "properties": {
          "page": {"type": "string", "description": "Page path, e.g. '/'."},
          "viewport": {"type": "string", "enum": ["desktop", "mobile"]},
          "severity": {"type": "string", "enum": ["error", "warning"]},
          "issue": {"type": "string", "description": "Objective description of the visual defect."},
          "fix": {"type": "string", "description": "Concrete CSS, HTML, or JS fix."}
        }
      }
    }
  }
}`),
	}
}

type visualFinding struct {
	Page     string `json:"page"`
	Viewport string `json:"viewport"`
	Severity string `json:"severity"`
	Issue    string `json:"issue"`
	Fix      string `json:"fix"`
}

func parseVisualAudit(content string) ([]visualFinding, error) {
	s := jsonrepair.Object(content)
	var reply struct {
		Items []visualFinding `json:"items"`
	}
	if err := json.Unmarshal([]byte(s), &reply); err != nil {
		return nil, fmt.Errorf("visual audit reply is not JSON: %w", err)
	}
	return reply.Items, nil
}

func visualDefectIssues(findings []visualFinding) []Issue {
	var out []Issue
	for _, f := range findings {
		if strings.ToLower(strings.TrimSpace(f.Severity)) != "error" {
			continue
		}
		issue := strings.TrimSpace(f.Issue)
		fix := strings.TrimSpace(f.Fix)
		if issue == "" {
			continue
		}
		page := strings.TrimSpace(f.Page)
		if page == "" {
			page = "/"
		}
		vp := strings.TrimSpace(f.Viewport)
		if vp == "" {
			vp = "desktop"
		}
		msg := fmt.Sprintf("[%s] %s", vp, issue)
		if fix != "" {
			msg += " Fix: " + fix
		}
		out = append(out, Issue{
			Severity:      store.SeverityError,
			Category:      visualDefectCategory,
			Deterministic: true,
			Page:          page,
			Signature:     fmt.Sprintf("%s|%s|%s|%d", visualDefectCategory, page, vp, len(out)),
			Message:       msg,
		})
		if len(out) >= maxVisualDefects {
			break
		}
	}
	return out
}

// runVisualAudit captures desktop and mobile screenshots of rendered project pages
// and runs a single VLM critique call when the review model supports vision and a
// headless browser is available. Any skip or failure returns (nil, false) without
// blocking the build.
func (w *Worker) runVisualAudit(ctx context.Context) ([]Issue, bool) {
	if w.bs().visualAudits >= maxVisualAudits {
		return nil, false
	}
	baseURL, hostHeader := w.probeTarget()
	if baseURL == "" {
		return nil, false
	}
	sess := w.newStageSession(StageReview)
	rp, err := sess.resolver.resolve()
	if err != nil || !supportsVision(rp) {
		return nil, false
	}

	shots, err := captureScreenshotsFn(w.validationContext(ctx), w.pdb.Reader(), baseURL, hostHeader, maxVisualAuditPages)
	if err != nil || len(shots) == 0 {
		return nil, false
	}
	w.bs().visualAudits++

	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Project brief:\n%s\n\nScreenshots attached:\n", strings.TrimSpace(w.buildBrief()))
	images := make([]llm.MessageImage, 0, len(shots))
	for i, s := range shots {
		if s.Viewport == "desktop" && (s.Path == "/" || len(w.bs().previewPNG) == 0) {
			w.bs().previewPNG = s.PNG
		}
		fmt.Fprintf(&prompt, "- Image %d: %s (%s)\n", i+1, s.Path, s.Viewport)
		images = append(images, llm.MessageImage{
			MIMEType: "image/png",
			Data:     s.PNG,
		})
	}

	findings, res, err := runStructuredCall(ctx, w, structuredCallOpts{
		Stage:         StageReview,
		System:        visualAuditSystem,
		Messages:      []llm.Message{{Role: llm.RoleUser, Content: prompt.String(), Images: images}},
		Schema:        visualAuditSchema(),
		MaxTokens:     4096,
		Timeout:       3 * time.Minute,
		Thinking:      &reviewThinkingDefault,
		ThinkingStage: StageReview,
		NoReplayCache: true,
	}, parseVisualAudit)
	if err != nil {
		w.logger.Warn("visual audit failed, skipping it", "error", err)
		return nil, false
	}
	issues := visualDefectIssues(findings)
	w.logger.Info("visual audit", "screenshots", len(shots), "defects", len(issues))
	if w.run != nil {
		w.runlog.entry(w.run.ID, RunLogIssue, "visual_audit", map[string]any{"findings": findings}, res.InputTokens, res.OutputTokens, 0)
	}
	return issues, true
}
