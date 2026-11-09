package snapshotbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// entityDescriptor is one snapshot entity's identity: a stable, human-readable
// key for the per-entity map, and the exact byte sequence that defines the entity
// for fingerprinting. entityList emits these in a fixed order; both the
// whole-snapshot Fingerprint and the per-entity map derive from that one
// enumeration, so they can never disagree about what changed.
type entityDescriptor struct {
	key  string
	desc string
}

// fpBuf accumulates one entity's fingerprint bytes. The separators carry the
// meaning, not the text: a NUL ends a field and a low tag byte closes a group,
// so two entities whose fields concatenate to the same characters still
// fingerprint differently. Byte-for-byte stability outranks tidiness: the
// termination differences between entities are load-bearing, not oversights to
// normalize away.
type fpBuf struct{ b strings.Builder }

// newFP starts an entity's bytes with its type prefix, which is glued to the
// first field rather than separated from it.
func newFP(prefix string) *fpBuf {
	f := &fpBuf{}
	f.b.WriteString(prefix)
	return f
}

// field writes a NUL-terminated field.
func (f *fpBuf) field(s string) *fpBuf { f.b.WriteString(s); f.b.WriteByte(0); return f }

// bare writes a field with no terminator, for an entity whose only field the
// prefix already separates.
func (f *fpBuf) bare(s string) *fpBuf { f.b.WriteString(s); return f }

// list writes each element NUL-terminated. An empty list writes nothing, so a
// group terminator is what tells "no elements" from "no group".
func (f *fpBuf) list(ss []string) *fpBuf {
	for _, s := range ss {
		f.field(s)
	}
	return f
}

// mark closes a group.
func (f *fpBuf) mark(tag byte) *fpBuf { f.b.WriteByte(tag); return f }

// markIf closes a group only when cond holds: the tag's PRESENCE is the bit.
func (f *fpBuf) markIf(cond bool, tag byte) *fpBuf {
	if cond {
		f.b.WriteByte(tag)
	}
	return f
}

// flag writes 1 or 0. Unlike markIf this byte is always present: it is a field's
// value, not a group boundary.
func (f *fpBuf) flag(on bool) *fpBuf {
	if on {
		f.b.WriteByte(1)
	} else {
		f.b.WriteByte(0)
	}
	return f
}

// sheet writes one stylesheet's names-only surface. A global stylesheet and a
// page's own stylesheet encode identically and differ only in what wraps them, so
// the encoding lives here once rather than in both entities where the two copies
// could diverge without anything saying so.
func (f *fpBuf) sheet(sc StyleContract) *fpBuf {
	vals := make([]string, 0, len(sc.Vars))
	for _, v := range sc.Vars {
		vals = append(vals, v+"="+sc.VarValues[v])
	}
	return f.field(sc.Path).
		list(vals).mark(1).
		list(sc.Classes).mark(2).
		list(sc.Keyframes).mark(3).
		markIf(sc.Truncated, 4)
}

func (f *fpBuf) String() string { return f.b.String() }

// entityList enumerates every snapshot entity with its fingerprint bytes, in the
// fixed order pages, layouts, components, tables, views, endpoints, files, orphans,
// modules. The byte sequences mirror each field that materially affects what the
// LLM sees, so an export added, an import retargeted, a table seeded, or a file's
// reachability flipping all change the relevant entity's descriptor.
func (s *ProjectSnapshot) entityList() []entityDescriptor {
	if s == nil {
		return nil
	}
	var out []entityDescriptor
	add := func(key string, f *fpBuf) { out = append(out, entityDescriptor{key, f.String()}) }

	for _, p := range s.Pages {
		add("page:"+p.Path, newFP("p:").field(p.Path).field(p.Title).list(p.Assets).mark(1))
	}
	for _, l := range s.Layouts {
		add("layout:"+l.Name, newFP("l:").bare(l.Name).mark(1))
	}
	for _, c := range s.Components {
		add("component:"+c.Name, newFP("c:").bare(c.Name).mark(1))
	}
	for _, t := range s.Tables {
		// A coarse empty/seeded bit (not the exact count) so the empty->seeded
		// transition re-renders the snapshot while routine row-count drift does
		// not churn the prompt cache.
		add("table:"+t.Name, newFP("t:").field(t.Name).list(t.Columns).markIf(t.RowCount > 0, 2).mark(1))
	}
	for _, v := range s.Views {
		add("view:"+v.Name, newFP("v:").field(v.Name).list(v.Columns).mark(1))
	}
	for _, e := range s.Endpoints {
		// WSMode/TickHz/HasReducer are fingerprinted because they are RENDERED: a
		// fact the row shows but the bytes omit cannot produce a delta, so the
		// model would read a stale mode with nothing marking it changed.
		// HasReducer is a BIT, not the reducer's length: "is it attached" is the
		// question the model cannot answer from anywhere else, while a length
		// would re-fingerprint the endpoint on every edit of a body it wrote.
		add("endpoint:"+e.AuthKind+":"+e.Methods+":"+e.Path,
			newFP("e:").field(e.Path).field(e.Methods).field(e.AuthKind).flag(e.Auth).
				field(e.WSMode).field(fmt.Sprintf("%d", e.TickHz)).flag(e.HasReducer))
	}
	for _, l := range s.Libraries {
		add("library:"+l.Name, newFP("lib:").field(l.Name).field(l.URL))
	}
	for _, fr := range s.Files {
		add("file:"+fr.Path, newFP("f:").field(fr.Path).field(fr.Kind).field(fr.Scope))
	}
	// The orphan set, so the snapshot re-renders when reachability changes without
	// any other field moving - e.g. a patch adds an import that pulls a previously
	// unlinked module into a linked entry's graph.
	for _, p := range s.UnlinkedPageFiles {
		add("unlinked:"+p, newFP("u:").field(p))
	}
	// Each module's export/import surface, so the snapshot re-renders when a
	// contract changes even though no path-level field moved.
	for _, m := range s.Modules {
		d := newFP("m:").field(m.Path).list(m.Exports).mark(1)
		for _, im := range m.Imports {
			d.field(im.Spec).list(im.Names).mark(2)
		}
		// Everything moduleRecord renders beyond names: parameter lists, data shapes,
		// the bare specifier and the basename-collision flag. Left out, a changed
		// arity or field set emits no delta and the model keeps wiring against the
		// stale line until the next full baseline.
		var sigs, shapes []string
		for _, name := range m.Exports {
			if params, ok := m.Sigs[name]; ok {
				sigs = append(sigs, name+"("+strings.Join(params, ",")+")")
			}
			if keys, ok := m.Shapes[name]; ok {
				shapes = append(shapes, name+"{"+strings.Join(keys, ",")+"}")
			}
		}
		d.list(sigs).mark(3).list(shapes).mark(4).field(m.Bare).markIf(m.Collides, 5)
		add("module:"+m.Path, d)
	}
	// Each global stylesheet's design-system surface, so the snapshot re-renders
	// when a token/class/keyframe is added, removed, or renamed - while a
	// value-only or whitespace-only CSS edit (names unchanged) leaves the
	// descriptor identical and so does not churn the prompt cache.
	for _, sc := range s.Styles {
		add("style:"+sc.Path, newFP("s:").sheet(sc))
	}
	// Each page's DOM id surface, so the snapshot re-renders when the markup gains or
	// loses an id a script binds to (an added container flips a null wire to sound).
	for _, dc := range s.PageDOM {
		add("dom:"+dc.Path, newFP("dom:").field(dc.Path).list(dc.IDs).markIf(dc.Truncated, 1))
	}
	// Each page's own stylesheet surface, so a class added to (or renamed in) a page's
	// CSS re-renders that page's contract - and a value-only edit does not.
	for _, ps := range s.PageStyles {
		d := newFP("pagestyle:").field(ps.Page)
		for _, sc := range ps.Sheets {
			d.sheet(sc).mark(5)
		}
		add("pagestyle:"+ps.Page, d)
	}
	return out
}

// Fingerprint returns a stable short hash of the snapshot's structural content,
// used by the tool loop to skip re-rendering when nothing has changed. Covers
// page/table/endpoint/file identities, not timestamps, so equal-content
// snapshots across turns produce equal fingerprints. Derived from entityList so
// it stays in lockstep with the per-entity map.
func (s *ProjectSnapshot) Fingerprint() string {
	if s == nil {
		return ""
	}
	h := sha256.New()
	for _, e := range s.entityList() {
		h.Write([]byte(e.desc))
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// entityFingerprints hashes each entity's descriptor individually, keyed by its
// stable key, so the tool loop can diff two snapshots and tell the model exactly
// which entities changed during its task. Returns nil for an empty project.
func (s *ProjectSnapshot) entityFingerprints() map[string]string {
	list := s.entityList()
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]string, len(list))
	for _, e := range list {
		sum := sha256.Sum256([]byte(e.desc))
		out[e.key] = hex.EncodeToString(sum[:8])
	}
	return out
}
