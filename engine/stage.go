package engine

// The pluggable stage layer. A Stage is one named phase of the pipeline; the
// registry maps names to implementations and defaultPipeline is the ordered
// name list the drive loop walks. A stage that finishes normally returns
// Outcome{Advance: true} and the loop moves to the next pipeline entry, so
// adding a stage is: implement Stage (Name, Info, Run), RegisterStage it, and
// insert its name into defaultPipeline. The stage's wall-clock cap and its
// owner-facing start line travel with it in Info.
//
// Stage-specific wiring that still lives elsewhere, because it is genuinely
// per-stage content rather than plumbing: the session tuning in DefaultSpec
// (spec.go), the prompt frame under knowledge/stages/<name>/, and the env-var
// timeout override in cmd/openfabrica.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Outcome is what a stage run produced: advance to the next pipeline stage,
// jump to a named stage, or Done (the run is complete). An error return
// supersedes all of these.
type Outcome struct {
	// Advance moves to the stage after this one in defaultPipeline. It is the
	// normal way a stage hands off, so the order lives in one place.
	Advance bool
	// Next names a specific stage to run next, for a jump the pipeline order
	// does not express. Ignored when Advance is set.
	Next string
	Done bool
	// Blocked marks a run that FINISHED but must not go live (see
	// engine/blocking.go). Distinct from an error return, which means the stage
	// could not finish and earns a retry - a blocked build has nothing to retry.
	// Done stays true; the drive loop reads Blocked to end the run failed
	// instead of stamping stage=live on it.
	Blocked bool
}

// StageInfo is what the drive loop needs to know about a stage besides how to
// run it.
type StageInfo struct {
	// Timeout is the stage's default wall-clock cap inside the drive loop.
	// Deps.StageTimeouts overrides it per deployment; zero means no cap.
	Timeout time.Duration
	// StartLines are interchangeable owner-facing "starting this phase" chat
	// messages; one is picked per run. Empty keeps the stage silent.
	StartLines []string
}

// Stage is one pipeline phase. Run drives whatever the stage needs (usually
// one or more sessions) and reports where to go next. Implementations must be
// stateless: per-run state lives on the Worker and the Run row.
type Stage interface {
	Name() string
	Info() StageInfo
	Run(ctx context.Context, w *Worker, r *Run) (Outcome, error)
}

var (
	stageMu       sync.RWMutex
	stageRegistry = map[string]Stage{}
)

// RegisterStage adds a stage implementation. Later registrations with the
// same name override earlier ones (tests substitute fakes this way).
func RegisterStage(s Stage) {
	stageMu.Lock()
	defer stageMu.Unlock()
	stageRegistry[s.Name()] = s
}

// stageByName resolves one registered stage.
func stageByName(name string) (Stage, error) {
	stageMu.RLock()
	defer stageMu.RUnlock()
	s, ok := stageRegistry[name]
	if !ok {
		known := make([]string, 0, len(stageRegistry))
		for k := range stageRegistry {
			known = append(known, k)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("unknown stage %q (registered: %v)", name, known)
	}
	return s, nil
}

// stageInfo returns a registered stage's Info, or the zero value for an
// unknown name (no cap, no start line).
func stageInfo(name string) StageInfo {
	s, err := stageByName(name)
	if err != nil {
		return StageInfo{}
	}
	return s.Info()
}

// defaultPipeline is the stage order for a fresh build run. EXPAND self-skips
// to PLAN when it does not apply (ambition off is handled by entry-stage
// selection in Worker.resolveRun; change requests and already-expanded runs
// by the stage itself).
var defaultPipeline = []string{StageExpand, StagePlan, StageBuild}

// firstStage returns the pipeline's entry stage.
func firstStage() string { return defaultPipeline[0] }

// nextStage returns the stage after from in defaultPipeline, or "" when from is
// the last stage or not in the pipeline.
func nextStage(from string) string {
	for i, name := range defaultPipeline {
		if name == from && i+1 < len(defaultPipeline) {
			return defaultPipeline[i+1]
		}
	}
	return ""
}

// resolveNext turns a finished stage's Outcome into the name of the stage to
// run next ("" when the outcome names none).
func resolveNext(from string, o Outcome) string {
	if o.Advance {
		return nextStage(from)
	}
	return o.Next
}

func init() {
	RegisterStage(expandStage{})
	RegisterStage(planStage{})
	RegisterStage(buildStage{})
}
