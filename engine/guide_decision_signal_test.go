package engine

import (
	"strings"
	"testing"

	"openfabrica/pack"
)

// The terrain guides own no tool and no library, so before the decision signal
// they reached BUILD only if the planner also repeated the slug in plan.Guides -
// and a plan that locks `terrain representation` has already made the call in
// machine-readable form. A plan can lock smooth-heightfield terrain, list three.js,
// and still build the terrain with no terrain guide in context.

func decisionPlan(topic, choice string) *Plan {
	return &Plan{
		Decisions: []PlanDecision{{Topic: topic, Choice: choice}},
		ExternalLibraries: []LibrarySpec{{
			Name: "three.js core", ModuleName: "three",
			URL: "https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js",
		}},
	}
}

func TestTerrainDecisionImpliesItsGuide(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	for _, tc := range []struct {
		choice string
		want   string
		absent string
	}{
		{"smooth-heightfield", "heightfield_terrain", "voxel_worlds"},
		{"heightfield", "heightfield_terrain", "voxel_worlds"},
		{"voxel", "voxel_worlds", "heightfield_terrain"},
		{"blocky-voxel", "voxel_worlds", "heightfield_terrain"},
		// Wording drift in the recorded choice must not silently stop matching.
		{"Smooth Heightfield", "heightfield_terrain", "voxel_worlds"},
		{"smooth_heightfield", "heightfield_terrain", "voxel_worlds"},
		// Extra words ("heightfield terrain") must still route to the heightfield guide.
		{"heightfield terrain", "heightfield_terrain", "voxel_worlds"},
	} {
		t.Run(tc.choice, func(t *testing.T) {
			got := planGuideSlugs(decisionPlan("terrain representation", tc.choice), ps)
			if !got[tc.want] {
				t.Errorf("choice %q did not imply %q; got %v", tc.choice, tc.want, got)
			}
			// The smooth-vs-blocky call is one recorded choice: the rival must
			// never ride along, or the build gets both terrain models at once.
			if got[tc.absent] {
				t.Errorf("choice %q also pulled the rival guide %q", tc.choice, tc.absent)
			}
		})
	}
}

// TestGuideDecisionsAreUnambiguous: two guides must not claim the same locked
// choice. guideDecisionMap is 1:1 (one topic/choice names ONE guide that owns how
// to build it), so a duplicate would be silently clobbered by whichever guide
// parsed last, and a plan locking that choice would get the wrong guide. Also
// catches an entry that is missing its `/` and would never match anything.
func TestGuideDecisionsAreUnambiguous(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	owner := map[string]string{}
	for _, g := range pack.Capabilities(ps) {
		for _, d := range g.Decisions {
			topic, choice, ok := strings.Cut(d, "/")
			if !ok {
				t.Errorf("guide %q decision %q has no `topic/choice` separator - it can never match", g.Slug, d)
				continue
			}
			key := decisionKey(topic, choice)
			if prev, dup := owner[key]; dup {
				t.Errorf("decision %q is claimed by guides %q and %q - the map is 1:1, one is silently clobbered", key, prev, g.Slug)
				continue
			}
			owner[key] = g.Slug
		}
	}
}

// TestExclusiveGuidesResolveToOne: when rival architecture guides are both wanted,
// exactly one survives, and WHICH one follows the signal that reached it. Shipping
// both would hand BUILD two terrain models at once.
func TestExclusiveGuidesResolveToOne(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	both := func(a, b guideSignal) map[string]guideSignal {
		return map[string]guideSignal{"voxel_worlds": a, "heightfield_terrain": b}
	}
	for _, tc := range []struct {
		name  string
		in    map[string]guideSignal
		want  string
		loser string
	}{
		// Evidence ties, so exclusive_default decides. A brief that loosely says
		// "voxel" about smooth terrain must not get the cube world.
		{"tie goes to the default", both(signalNamed, signalNamed), "heightfield_terrain", "voxel_worlds"},
		// A locked decision outranks the default: the plan committed to cubes.
		{"locked beats the default", both(signalLocked, signalNamed), "voxel_worlds", "heightfield_terrain"},
		{"locked beats inferred", both(signalLocked, signalInferred), "voxel_worlds", "heightfield_terrain"},
		{"inferred beats named", both(signalNamed, signalInferred), "heightfield_terrain", "voxel_worlds"},
		// 2D vs 3D engine: an incoherent plan that declares both must resolve to one
		// rendering stack. On an even signal the slug-order backstop keeps three.js.
		{"2D and 3D resolve to one", map[string]guideSignal{"3d_rendering": signalNamed, "phaser_2d": signalNamed}, "3d_rendering", "phaser_2d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped := resolveExclusives(tc.in, ps, nil)
			if !kept[tc.want] {
				t.Errorf("want %q kept, got %v", tc.want, sortedSlugs(kept))
			}
			if kept[tc.loser] {
				t.Errorf("rival %q must be dropped, got %v", tc.loser, sortedSlugs(kept))
			}
			if len(dropped) != 1 || dropped[0] != tc.loser {
				t.Errorf("want dropped=[%s], got %v", tc.loser, dropped)
			}
		})
	}
	// A non-rival guide is never touched, whatever else is selected.
	kept, _ := resolveExclusives(map[string]guideSignal{
		"voxel_worlds": signalNamed, "heightfield_terrain": signalNamed, "payments": signalNamed,
	}, ps, nil)
	if !kept["payments"] {
		t.Errorf("an additive guide must survive exclusion resolution, got %v", sortedSlugs(kept))
	}
}

// TestExclusiveResolutionIsDeterministic: the survivor cannot depend on map
// iteration order, or the build seed's bytes would vary run to run and break the
// prompt cache.
func TestExclusiveResolutionIsDeterministic(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	in := map[string]guideSignal{"voxel_worlds": signalNamed, "heightfield_terrain": signalNamed, "3d_rendering": signalInferred}
	first, _ := resolveExclusives(in, ps, nil)
	for i := 0; i < 50; i++ {
		got, _ := resolveExclusives(in, ps, nil)
		if len(got) != len(first) {
			t.Fatalf("run %d differs in size: %v vs %v", i, sortedSlugs(got), sortedSlugs(first))
		}
		for slug := range first {
			if !got[slug] {
				t.Fatalf("run %d dropped %q; got %v", i, slug, sortedSlugs(got))
			}
		}
	}
}

// TestRequiredBaseSurvivesExclusion: a required base MAY now be a rival-group member
// (3d_rendering is the base of the 3D family and the rival of phaser_2d). planGuideSlugs
// resolves rivals and THEN closes over `requires:`, so a base dropped in resolution
// would be re-admitted afterwards and its rival would ride back in. The invariant that
// replaces the old "no exclusive base" rule: whenever a wanted guide requires an
// exclusive base, resolution must PROTECT it so the base wins even against a stronger-
// signalled rival, and the rival is dropped for good.
func TestRequiredBaseSurvivesExclusion(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	excl := guideExclusiveMap(ps)
	checked := 0
	for _, g := range pack.Capabilities(ps) {
		for _, base := range g.Requires {
			base = strings.TrimSpace(base)
			for _, rival := range sortedSlugs(excl[base]) {
				checked++
				want := map[string]guideSignal{
					g.Slug: signalInferred,
					base:   signalInferred,
					rival:  signalLocked, // even a stronger rival must lose to the base
				}
				kept, _ := resolveExclusives(want, ps, requiredBases(want, ps))
				if !kept[base] {
					t.Errorf("guide %q requires exclusive base %q, but the base was dropped for rival %q", g.Slug, base, rival)
				}
				if kept[rival] {
					t.Errorf("rival %q survived alongside protected base %q - both cores would be fed", rival, base)
				}
			}
		}
	}
	if checked == 0 {
		t.Skip("no required base is currently a rival member")
	}
}

// TestRivalRenderingCoresNotFedTogether guards against rival rendering stacks: a plan
// that declares BOTH a 3D engine (three) and a 2D engine (phaser) plus a locked 3D
// terrain decision. BUILD must receive exactly one rendering stack, never the
// contradictory Phaser and three.js cores at once.
func TestRivalRenderingCoresNotFedTogether(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	plan := &Plan{
		Decisions: []PlanDecision{{Topic: "terrain representation", Choice: "heightfield terrain"}},
		ExternalLibraries: []LibrarySpec{
			{Name: "three.js", ModuleName: "three", URL: "https://cdn.jsdelivr.net/npm/three@0.185.1/build/three.module.js"},
			{Name: "phaser", ModuleName: "phaser", URL: "https://cdn.jsdelivr.net/npm/phaser@3.90.0/dist/phaser.min.js"},
		},
	}
	got := planGuideSlugs(plan, ps)
	if !got["3d_rendering"] {
		t.Errorf("the 3D base must survive; got %v", sortedSlugs(got))
	}
	if !got["heightfield_terrain"] {
		t.Errorf("the locked terrain guide must be fed; got %v", sortedSlugs(got))
	}
	if got["phaser_2d"] {
		t.Errorf("the rival 2D core must not be fed alongside the 3D stack; got %v", sortedSlugs(got))
	}
}

// TestRivalCoreDroppedWhenBaseArrivesViaRequires is the sibling of the case above:
// the 3D signal comes from a locked terrain decision (voxel), which pulls 3d_rendering
// in only through the requires closure, while phaser is declared as a library. The base
// enters after rival resolution, so without protecting it the dropped 2D rival rides
// back in. BUILD must still get exactly one rendering stack.
func TestRivalCoreDroppedWhenBaseArrivesViaRequires(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	plan := &Plan{
		Decisions: []PlanDecision{{Topic: "terrain representation", Choice: "voxel"}},
		ExternalLibraries: []LibrarySpec{
			{Name: "phaser", ModuleName: "phaser", URL: "https://cdn.jsdelivr.net/npm/phaser@3.90.0/dist/phaser.min.js"},
		},
	}
	got := planGuideSlugs(plan, ps)
	if !got["3d_rendering"] {
		t.Errorf("the 3D base must survive; got %v", sortedSlugs(got))
	}
	if !got["voxel_worlds"] {
		t.Errorf("the locked terrain guide must be fed; got %v", sortedSlugs(got))
	}
	if got["phaser_2d"] {
		t.Errorf("the rival 2D core must not be fed alongside the 3D stack; got %v", sortedSlugs(got))
	}
}

// TestGameFeelDroppedWithoutRenderingGuide: game_feel is an add-on (requires_any_of a
// rendering guide). A plan that merely NAMES it but selects no game/canvas surface -
// e.g. a video-call app - must not get its core; a plan with a rendering guide
// keeps it.
func TestGameFeelDroppedWithoutRenderingGuide(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	bare := &Plan{Guides: []string{"game_feel"}}
	if planGuideSlugs(bare, ps)["game_feel"] {
		t.Error("game_feel must be dropped when no rendering guide is selected")
	}
	withRender := &Plan{
		Guides: []string{"game_feel"},
		ExternalLibraries: []LibrarySpec{
			{Name: "three.js", ModuleName: "three", URL: "https://cdn.jsdelivr.net/npm/three@0.185.1/build/three.module.js"},
		},
	}
	if !planGuideSlugs(withRender, ps)["game_feel"] {
		t.Error("game_feel must be kept when a rendering guide is selected")
	}
	// A vanilla-canvas game selects no engine guide; the locked rendering decision is
	// its anchor. The PLAN frame tells the planner to declare game_feel for any game.
	vanilla := &Plan{
		Guides:    []string{"game_feel"},
		Decisions: []PlanDecision{{Topic: "Rendering approach", Choice: "vanilla <canvas> + requestAnimationFrame"}},
	}
	if !planGuideSlugs(vanilla, ps)["game_feel"] {
		t.Error("game_feel must be kept for a game that locked a vanilla-canvas rendering approach")
	}
	// A non-canvas rendering choice is not an anchor.
	dom := &Plan{
		Guides:    []string{"game_feel"},
		Decisions: []PlanDecision{{Topic: "rendering approach", Choice: "plain DOM"}},
	}
	if planGuideSlugs(dom, ps)["game_feel"] {
		t.Error("a DOM rendering decision must not anchor game_feel")
	}
}

// TestDecisionAliasTolerantMatch: a locked choice that misses the exact map by extra
// words still routes when exactly one guide's alias tokens are all present, and refuses
// to guess when two rival guides could both match.
func TestDecisionAliasTolerantMatch(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	aliases := guideDecisionAliases(ps)
	if got := matchDecisionAlias(aliases, "terrain representation", "rolling heightfield surface"); got != "heightfield_terrain" {
		t.Errorf("extra-word heightfield choice did not route: got %q", got)
	}
	if got := matchDecisionAlias(aliases, "terrain representation", "blocky voxel chunks"); got != "voxel_worlds" {
		t.Errorf("extra-word voxel choice did not route: got %q", got)
	}
	if got := matchDecisionAlias(aliases, "terrain representation", "voxel over smooth heightfield"); got != "" {
		t.Errorf("a choice matching both rivals must not route, got %q", got)
	}
}

// An unrelated decision must not route anything: the map is deliberately narrow,
// covering only guides with no other signal.
func TestUnmappedDecisionImpliesNoTerrainGuide(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	got := planGuideSlugs(decisionPlan("persistence", "client"), ps)
	for _, slug := range []string{"heightfield_terrain", "voxel_worlds"} {
		if got[slug] {
			t.Errorf("an unrelated decision pulled %q", slug)
		}
	}
}
