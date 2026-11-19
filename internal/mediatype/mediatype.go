// Package mediatype centralizes file extension <-> MIME type mappings,
// executable/scriptable content deny-lists, and downloadable asset policies.
// It is a leaf package with no module-internal imports.
package mediatype

import (
	"path/filepath"
	"strings"
)

// ContentTypeByFilename returns the canonical MIME type for filename based on
// its extension, or "" when the extension is unknown so callers can fall back
// to stored metadata or octet-stream.
func ContentTypeByFilename(filename string) string {
	return ContentTypeByExt(filepath.Ext(filename))
}

// ContentTypeByExt returns the canonical MIME type for a file extension
// (case-insensitive, with or without leading dot), or "" when unknown.
func ContentTypeByExt(ext string) string {
	switch normalizeExt(ext) {
	case ".css":
		return "text/css"
	case ".js", ".mjs":
		return "application/javascript"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".ico":
		return "image/x-icon"
	case ".xml":
		return "application/xml"
	case ".pdf":
		return "application/pdf"
	case ".csv":
		return "text/csv"
	case ".txt":
		return "text/plain"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".m4a":
		return "audio/mp4"
	case ".glb":
		return "model/gltf-binary"
	case ".gltf":
		return "model/gltf+json"
	default:
		return ""
	}
}

// dangerousUploadMIMEs lists executable/runnable MIME types rejected on upload
// and download endpoints.
var dangerousUploadMIMEs = map[string]bool{
	"application/x-msdownload":                      true,
	"application/x-msdos-program":                   true,
	"application/x-executable":                      true,
	"application/x-mach-binary":                     true,
	"application/x-dosexec":                         true,
	"application/x-sh":                              true,
	"application/x-bat":                             true,
	"application/x-bsh":                             true,
	"application/x-csh":                             true,
	"application/java-archive":                      true,
	"application/vnd.microsoft.portable-executable": true,
}

// dangerousDownloadExts lists extensions that must never be accepted by
// manage_web_fetch download, even if listed in operator config: executables,
// shell scripts, and active HTML/JS payloads that would execute if served.
var dangerousDownloadExts = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".msi": true,
	".bat": true, ".cmd": true, ".sh": true, ".bash": true, ".zsh": true,
	".csh": true, ".ps1": true, ".vbs": true, ".jar": true, ".app": true,
	".com": true, ".scr": true, ".pif": true, ".apk": true, ".deb": true,
	".rpm": true, ".html": true, ".htm": true, ".xhtml": true,
	".js": true, ".mjs": true, ".cjs": true, ".php": true, ".asp": true,
	".aspx": true, ".jsp": true, ".cgi": true,
}

// IsDangerousMIME reports whether mediaType represents an executable or
// runnable binary type.
func IsDangerousMIME(mediaType string) bool {
	lower := strings.ToLower(strings.TrimSpace(mediaType))
	if dangerousUploadMIMEs[lower] {
		return true
	}
	return strings.Contains(lower, "executable") || strings.Contains(lower, "x-msi")
}

// IsDangerousDownloadExt reports whether ext is on the hardcoded security
// deny-list for downloaded assets.
func IsDangerousDownloadExt(ext string) bool {
	return dangerousDownloadExts[normalizeExt(ext)]
}

// defaultDownloadAssetKinds maps built-in safe asset extensions to their kind.
var defaultDownloadAssetKinds = map[string]string{
	".png":   "image",
	".jpg":   "image",
	".jpeg":  "image",
	".webp":  "image",
	".gif":   "image",
	".svg":   "image",
	".ico":   "image",
	".glb":   "model",
	".gltf":  "model",
	".obj":   "model",
	".mtl":   "model",
	".mp3":   "audio",
	".wav":   "audio",
	".ogg":   "audio",
	".m4a":   "audio",
	".mp4":   "video",
	".webm":  "video",
	".woff":  "font",
	".woff2": "font",
	".ttf":   "font",
	".otf":   "font",
	".pdf":   "document",
	".csv":   "data",
	".json":  "data",
	".txt":   "data",
}

// ClassifyDownloadExt resolves ext against the built-in safe asset list and
// optional operator-configured extra extensions. Extensions on the security
// deny-list are always rejected regardless of extra.
func ClassifyDownloadExt(ext string, extra []string) (kind string, allowed bool) {
	norm := normalizeExt(ext)
	if norm == "" || IsDangerousDownloadExt(norm) {
		return "", false
	}
	if k, ok := defaultDownloadAssetKinds[norm]; ok {
		return k, true
	}
	for _, e := range extra {
		if normalizeExt(e) == norm {
			return "custom", true
		}
	}
	return "", false
}

// IsAllowedDownloadContentType verifies that the HTTP response Content-Type
// matches the expected asset kind and is not an HTML/script/executable payload.
func IsAllowedDownloadContentType(kind, mediaType string) bool {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if mt == "text/html" || mt == "application/xhtml+xml" || strings.Contains(mt, "javascript") || IsDangerousMIME(mt) {
		return false
	}
	switch kind {
	case "image":
		return strings.HasPrefix(mt, "image/")
	case "audio":
		return strings.HasPrefix(mt, "audio/") || mt == "application/ogg" || mt == "application/octet-stream"
	case "video":
		return strings.HasPrefix(mt, "video/") || mt == "application/octet-stream"
	case "model":
		return strings.HasPrefix(mt, "model/") || mt == "application/octet-stream" || mt == "application/json" || mt == "text/plain"
	case "font":
		return strings.HasPrefix(mt, "font/") || strings.Contains(mt, "font") || mt == "application/octet-stream"
	case "document":
		return mt == "application/pdf" || mt == "application/octet-stream"
	case "data":
		return strings.HasPrefix(mt, "text/") || mt == "application/json" || mt == "application/csv" || mt == "application/octet-stream"
	case "custom":
		return mt != ""
	default:
		return false
	}
}

func normalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}
