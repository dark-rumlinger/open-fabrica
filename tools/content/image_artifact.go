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

	storagePath, err := store.WriteFileToDisk(ctx.DataDirOrDefault(), ctx.ProjectID, "assets", filename, data)
	if err != nil {
		return "", fmt.Errorf("writing to disk: %w", err)
	}

	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, _ := json.Marshal(meta)

	_, err = ctx.DB.Exec(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, scope, metadata, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', ?, ?, ?, 'global', ?, 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, path) DO UPDATE SET
		   storage = 'disk',
		   body = '',
		   content_type = excluded.content_type,
		   size = excluded.size,
		   disk_path = excluded.disk_path,
		   scope = excluded.scope,
		   metadata = excluded.metadata,
		   is_deleted = 0,
		   deleted_at = NULL,
		   updated_at = CURRENT_TIMESTAMP`,
		filename, contentType, len(data), storagePath, string(metaJSON),
	)
	if err != nil {
		return "", fmt.Errorf("recording artifact: %w", err)
	}
	return storagePath, nil
}
