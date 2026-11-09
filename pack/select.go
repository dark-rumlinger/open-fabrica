package pack

// Selection is the result of one selection pass over the effective packs for a
// stage: the stage frame, the fragments that attach to it, and the full
// capability-guide set (rendered as an always-on index, never inlined).
type Selection struct {
	Stage     *Pack  // the role:stage frame for this stage (nil if none)
	Fragments []Pack // role:fragment packs to append to the system prompt
	Guides    []Pack // every role:capability pack, for the always-on guide index
}

// Select partitions the effective packs for a stage: the stage frame and
// fragments, plus the whole guide set for the index. It inlines no guide body;
// which cores BUILD inlines is the engine's call (engine/guide_prefetch.go).
// Deterministic - order follows the input (position, slug).
func Select(ps []Pack, stage string) Selection {
	var out Selection
	for i := range ps {
		p := ps[i]
		switch p.Role {
		case RoleStage:
			if out.Stage == nil && (p.Stage == stage || p.Slug == stage) {
				sp := p
				out.Stage = &sp
			}
		case RoleFragment:
			if fragmentApplies(p, stage) {
				out.Fragments = append(out.Fragments, p)
			}
		case RoleCapability:
			out.Guides = append(out.Guides, p)
		}
	}
	return out
}

// fragmentApplies reports whether a role:fragment pack attaches to this stage:
// its `stages` list names the stage (e.g. _common_rules -> [build, live]). A
// fragment with no `stages` list attaches nowhere.
func fragmentApplies(p Pack, stage string) bool {
	for _, s := range p.Stages {
		if s == stage {
			return true
		}
	}
	return false
}
