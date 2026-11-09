// Package knowledge is the single embedded home for everything the engine feeds
// the model about the platform:
//
//   - mental-model.md          the always-on platform overview (rides every prompt)
//   - guides/<cat>/<slug>/GUIDE.md   the recall-on-demand capability guides
//   - contracts/*.contract.md  the canonical load-bearing facts (transcluded)
//   - stages/<stage>/STAGE.md  the per-stage frames + the _common_rules fragment
//
// The Go logic lives in the pack (selection/assembly) and contracts (rendering)
// packages; this package only embeds and serves the content, so the knowledge tree
// is ONE directory developers edit and ONE fs.FS the engine reads. Serving is pure
// and deterministic (the prompt-cache contract).
//
// What ships in the binary is encrypted. Developers edit the plaintext tree
// (mental-model.md, guides/, stages/, contracts/); `go generate ./knowledge`
// mirrors it into encrypted/ as AES-GCM ciphertext (see cmd encgen and
// internal/mdcrypt), and only encrypted/ is embedded. FS below decrypts on read
// and presents the exact same virtual paths, so no consumer of this package
// changed. This keeps the prompt text out of a `strings` dump; it is bar-raising
// against casual extraction, not an unbreakable secret (the key ships too).
package knowledge

import (
	"bytes"
	"embed"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"openfabrica/internal/mdcrypt"
)

//go:generate go run ./encgen

// encFS holds the encrypted knowledge tree. `all:` is required so the underscore-
// prefixed _common_rules stage and any dotfiles are embedded too.
//
//go:embed all:encrypted
var encFS embed.FS

// FS is the knowledge tree as the rest of the program sees it: plaintext markdown
// at the same virtual paths the old cleartext embed exposed. It wraps the
// encrypted embed, strips the "encrypted" prefix, and decrypts file contents on
// read. Directory listings pass through unchanged, so names and structure are
// identical to before. It satisfies fs.FS, fs.ReadFileFS, fs.ReadDirFS and
// fs.StatFS, plus the concrete ReadFile/ReadDir methods callers already use.
var FS = decryptFS{root: encFS, prefix: "encrypted"}

type decryptFS struct {
	root   embed.FS
	prefix string
}

// phys maps a virtual path (e.g. "contracts/x.contract.md") to its physical
// location in the embedded tree (e.g. "encrypted/contracts/x.contract.md").
func (d decryptFS) phys(name string) string {
	if name == "." {
		return d.prefix
	}
	return path.Join(d.prefix, name)
}

func (d decryptFS) ReadFile(name string) ([]byte, error) {
	blob, err := d.root.ReadFile(d.phys(name))
	if err != nil {
		return nil, err
	}
	pt, err := mdcrypt.Decrypt(blob)
	if err != nil {
		return nil, &fs.PathError{Op: "decrypt", Path: name, Err: err}
	}
	return pt, nil
}

func (d decryptFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return d.root.ReadDir(d.phys(name))
}

func (d decryptFS) Stat(name string) (fs.FileInfo, error) {
	info, err := fs.Stat(d.root, d.phys(name))
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return dirInfo{name: path.Base(name), modTime: info.ModTime()}, nil
	}
	// Size must reflect the decrypted length, so read and decrypt to measure it.
	pt, err := d.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return fileInfo{name: path.Base(name), size: int64(len(pt)), modTime: info.ModTime()}, nil
}

func (d decryptFS) Open(name string) (fs.File, error) {
	// Directories (including ".") are served straight from the embedded tree so
	// ReadDir/WalkDir see the real structure; only regular files are decrypted.
	f, err := d.root.Open(d.phys(name))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.IsDir() {
		return f, nil
	}
	_ = f.Close()
	pt, err := d.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return &openFile{
		info:   fileInfo{name: path.Base(name), size: int64(len(pt)), modTime: info.ModTime()},
		reader: bytes.NewReader(pt),
	}, nil
}

type fileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (fi fileInfo) Name() string       { return fi.name }
func (fi fileInfo) Size() int64        { return fi.size }
func (fi fileInfo) Mode() fs.FileMode  { return 0o444 }
func (fi fileInfo) ModTime() time.Time { return fi.modTime }
func (fi fileInfo) IsDir() bool        { return false }
func (fi fileInfo) Sys() any           { return nil }

type dirInfo struct {
	name    string
	modTime time.Time
}

func (di dirInfo) Name() string       { return di.name }
func (di dirInfo) Size() int64        { return 0 }
func (di dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (di dirInfo) ModTime() time.Time { return di.modTime }
func (di dirInfo) IsDir() bool        { return true }
func (di dirInfo) Sys() any           { return nil }

type openFile struct {
	info   fileInfo
	reader *bytes.Reader
}

func (f *openFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *openFile) Read(b []byte) (int, error) { return f.reader.Read(b) }
func (f *openFile) Close() error               { return nil }

// MentalModel returns the always-on platform overview (mental-model.md): the
// block the assembler renders into every stage's system prompt. Its
// `{{contract:<id>}}` tokens are transcluded and its `{{GUIDE_INDEX}}` token is
// replaced by the assembler, so the bytes here are stable input, not output.
func MentalModel() (string, error) {
	b, err := FS.ReadFile("mental-model.md")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CategoryNotes returns the optional one-line description for each guide
// category, keyed by category directory name (which equals the guides'
// `category` front-matter). Read from guides/<category>/CATEGORY.md; a category
// with no CATEGORY.md is simply absent. Deterministic (sorted scan).
func CategoryNotes() (map[string]string, error) {
	entries, err := FS.ReadDir("guides")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make(map[string]string, len(names))
	for _, name := range names {
		b, err := FS.ReadFile("guides/" + name + "/CATEGORY.md")
		if err != nil {
			continue // CATEGORY.md is optional
		}
		if note := strings.TrimSpace(string(b)); note != "" {
			out[name] = note
		}
	}
	return out, nil
}

// PlatformLawIDs returns the contract ids transcluded under mental-model.md's
// "## Platform laws" heading, in document order. The mental model is the single
// home for which wire shapes are load-bearing enough to ride every prompt; any
// other surface that must carry the same set (owner chat drives the same edit
// tools) reads it from here instead of keeping a parallel list. Deterministic.
func PlatformLawIDs() ([]string, error) {
	body, err := MentalModel()
	if err != nil {
		return nil, err
	}
	const heading = "## Platform laws"
	i := strings.Index(body, heading)
	if i < 0 {
		return nil, nil
	}
	section := body[i+len(heading):]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}
	var ids []string
	const open = "{{contract:"
	for {
		k := strings.Index(section, open)
		if k < 0 {
			return ids, nil
		}
		rest := section[k+len(open):]
		e := strings.Index(rest, "}}")
		if e < 0 {
			return ids, nil
		}
		ids = append(ids, strings.TrimSpace(rest[:e]))
		section = rest[e+2:]
	}
}
