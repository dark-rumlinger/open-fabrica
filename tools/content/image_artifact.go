package content

import (
	"encoding/json"
	"fmt"

	"openfabrica/store"
	"openfabrica/tools"
)

// SaveImageBytesAsArtifact persists an in-memory image as a kind=file
// row with scope=global. Single sink for "I produced image bytes, store
// them" used by both manage_stockphotos (Pexels download) and
// manage_image_gen (AI synthesis), so the on-disk layout and DB shape stay
// identical regardless of where the bytes came from.
//
// Returns the disk_path of the written file. The artifact is upserted on
// (kind=file, path); re-saving with the same filename overwrites the
// previous bytes.
func SaveImageBytesAsArtifact(ctx *tools.ToolContext, data []byte, contentType, filename, altText string) (string, error) {
	var meta map[string]any
	if altText != "" {
		meta = map[string]any{"alt_text": altText}
	}
	return SaveBytesAsArtifact(ctx, data, contentType, filename, meta)
}

// SaveBytesAsArtifact is the content-type-agnostic sink for "I produced bytes,
// store them as a served file". It writes the bytes to disk and upserts a
// kind=file, scope=global of_files row served at /files/<filename>. Images
// (SaveImageBytesAsArtifact) and generated documents (manage_documents) both
// route through here so the on-disk layout and DB shape stay identical
// regardless of payload kind. Returns the disk_path of the written file.
func SaveBytesAsArtifact(ctx *tools.ToolContext, data []byte, contentType, filename string, meta map[string]any) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("no bytes to save")
	}
	if filename == "" {
		return "", fmt.Errorf("filename is required")
	}

	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, _ := json.Marshal(meta)

	return store.SaveDiskArtifact(ctx.DB, ctx.DataDirOrDefault(), ctx.ProjectID, "assets", filename, contentType, data, string(metaJSON))
}
