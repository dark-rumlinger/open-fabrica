package analysis

// Shared export-surface helpers: a "who exports what" index over a set of module
// bodies, and a relative-specifier suggester for fix messages. Used by the
// cross-module "did you mean" hint on an unresolved import and by the
// dynamic-import destructuring check.

import (
	"path"
	"sort"
	"strings"
)

// ExportIndex maps each named export to the sorted, deduped list of project module
// paths that export it, across every module in bodies. Bodies are comment-stripped
// so a commented-out `export` never registers.
func ExportIndex(bodies map[string]string) map[string][]string {
	tmp := map[string]map[string]bool{}
	for p, b := range bodies {
		for _, name := range ExtractNamedExports(StripJSComments(b)) {
			if tmp[name] == nil {
				tmp[name] = map[string]bool{}
			}
			tmp[name][p] = true
		}
	}
	out := make(map[string][]string, len(tmp))
	for name, set := range tmp {
		paths := make([]string, 0, len(set))
		for p := range set {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		out[name] = paths
	}
	return out
}

// ModuleSpecifierIndex builds the bare-specifier index (specifier -> project
// path) over the module set of bodies - the byName argument the fix-message
// helpers resolve import specifiers through. One call per rejection/sweep, not
// per name.
func ModuleSpecifierIndex(bodies map[string]string) map[string]string {
	paths := make([]string, 0, len(bodies))
	for p := range bodies {
		paths = append(paths, p)
	}
	byName, _ := ProjectModuleMap(paths)
	return byName
}

// SuggestImportSpecifier returns the ES module specifier a fix message or an
// auto-wired import line should use to reach `to` from `from`. The bare logical
// name (the importmap form) is THE import style and is returned whenever byName
// resolves it to `to`; only a basename collision falls back to the relative path.
func SuggestImportSpecifier(from, to string, byName map[string]string) string {
	if name := ProjectModuleName(to); name != "" && byName[name] == to {
		return name
	}
	fromDir := path.Dir(from)
	toDir := path.Dir(to)
	base := path.Base(to)
	if fromDir == toDir {
		return "./" + base
	}
	fp := splitPathSegments(fromDir)
	tp := splitPathSegments(toDir)
	i := 0
	for i < len(fp) && i < len(tp) && fp[i] == tp[i] {
		i++
	}
	seg := make([]string, 0, len(fp)+len(tp))
	for j := i; j < len(fp); j++ {
		seg = append(seg, "..")
	}
	seg = append(seg, tp[i:]...)
	seg = append(seg, base)
	spec := strings.Join(seg, "/")
	if !strings.HasPrefix(spec, ".") {
		spec = "./" + spec
	}
	return spec
}

func splitPathSegments(dir string) []string {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return nil
	}
	return strings.Split(dir, "/")
}
