package content

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// manage_media, image resize / thumbnail / optimize. Reads from of_files,
// writes back through mediaRegisterOutput so processed images are recorded.

const maxMediaInputSize = 10 * 1024 * 1024 // 10MB

// maxMediaOutputPixels caps the pixel count of a generated image. resizeImage /
// cropCenter allocate an RGBA buffer of 4*W*H bytes, so an unbounded width/height
// (or thumbnail size) would let one call request a multi-GB allocation and
// OOM-crash the process. 40 MP is ~160 MB as RGBA, comfortably above any real
// hero/print image while keeping a single request bounded.
const maxMediaOutputPixels = 40_000_000

// maxMediaSide is a per-side ceiling. Besides being a sane bound on its own, it
// keeps width*height from overflowing int in mediaDimsExceedCap (the pixel-count
// term is only evaluated once both sides are known to be <= this).
const maxMediaSide = 20_000

// mediaDimsExceedCap reports whether an output of w x h pixels exceeds the
// allocation caps. The per-side checks short-circuit before the multiply, so the
// product can never overflow.
func mediaDimsExceedCap(w, h int) bool {
	return w > maxMediaSide || h > maxMediaSide || int64(w)*int64(h) > maxMediaOutputPixels
}

// mediaDirName maps user-facing storage names to on-disk directory names.
func mediaDirName(storage string) string {
	if storage == "assets" {
		return "assets"
	}
	return "files"
}

// loadImage reads and decodes an image from storage, returning the image and its format.
func mediaLoadImage(dataDir string, projectID int, storage, filename string) (image.Image, string, error) {
	// Contain the read to the project's storage dir: reject ../ traversal and
	// absolute paths before building the path, so a crafted source cannot read
	// another project's files or arbitrary host files.
	if err := store.ValidateStorageFilename(filename); err != nil {
		return nil, "", err
	}
	dir, _ := store.StorageDir(dataDir, projectID, mediaDirName(storage))
	path := filepath.Join(dir, filename)

	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("file not found: %s", filename)
	}
	if info.Size() > maxMediaInputSize {
		return nil, "", fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxMediaInputSize)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("opening file: %w", err)
	}
	defer func() { _ = f.Close() }()

	img, format, err := image.Decode(f)
	if err != nil {
		return nil, "", fmt.Errorf("decoding image (only JPEG/PNG supported): %w", err)
	}

	return img, format, nil
}

// saveImage encodes and writes an image to storage, returning the output path.
func mediaSaveImage(dataDir string, projectID int, storage, filename string, img image.Image, format string, quality int) (string, int64, error) {
	var buf bytes.Buffer
	var err error
	switch format {
	case "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	case "png":
		err = png.Encode(&buf, img)
	default:
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	}
	if err != nil {
		return "", 0, fmt.Errorf("encoding image: %w", err)
	}

	// Route through the shared, traversal-guarded writer instead of a raw
	// os.Create on a caller-supplied path: WriteFileToDisk rejects ../ and
	// absolute paths, so out_path cannot escape the project's storage dir.
	outPath, err := store.WriteFileToDisk(dataDir, projectID, mediaDirName(storage), filename, buf.Bytes())
	if err != nil {
		return "", 0, err
	}
	return outPath, int64(buf.Len()), nil
}

// registerOutput inserts the output image into of_files. The bytes are
// already on disk; without this row, ServeFile can't locate them and the
// returned URL will 404, so the insert failure is fatal to the caller.
func mediaRegisterOutput(ctx *tools.ToolContext, storage, filename, contentType string, size int64) error {
	subdir := "files"
	if storage == "assets" {
		subdir = "assets"
	}
	diskPath := filepath.Join(ctx.ProjectDir(), subdir, filepath.FromSlash(filename))
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, scope, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', ?, ?, ?, 'global', 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, path) DO UPDATE SET
		   storage = 'disk',
		   body = '',
		   content_type = excluded.content_type,
		   size = excluded.size,
		   disk_path = excluded.disk_path,
		   updated_at = CURRENT_TIMESTAMP`,
		filename, contentType, size, diskPath,
	); err != nil {
		return fmt.Errorf("registering media output %q: %w", filename, err)
	}
	return nil
}

// resizeImage scales an image to fit within target dimensions using nearest-neighbor.
func resizeImage(src image.Image, targetW, targetH int) image.Image {
	srcBounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	draw.Draw(dst, dst.Bounds(), image.Transparent, image.Point{}, draw.Src)

	srcW := srcBounds.Dx()
	srcH := srcBounds.Dy()

	for y := 0; y < targetH; y++ {
		srcY := y * srcH / targetH
		for x := 0; x < targetW; x++ {
			srcX := x * srcW / targetW
			dst.Set(x, y, src.At(srcBounds.Min.X+srcX, srcBounds.Min.Y+srcY))
		}
	}

	return dst
}

// cropCenter extracts a centered square crop from an image.
func cropCenter(src image.Image, size int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// Determine crop rectangle.
	cropSize := w
	if h < cropSize {
		cropSize = h
	}

	x0 := bounds.Min.X + (w-cropSize)/2
	y0 := bounds.Min.Y + (h-cropSize)/2

	dst := image.NewRGBA(image.Rect(0, 0, cropSize, cropSize))
	draw.Draw(dst, dst.Bounds(), src, image.Point{X: x0, Y: y0}, draw.Src)

	// Then resize to target size.
	if cropSize != size {
		return resizeImage(dst, size, size)
	}
	return dst
}

func mediaResize(ctx *tools.ToolContext, source string, width, height int, output string) (*tools.Result, error) {
	if width <= 0 {
		return tools.Fail("width is required and must be positive"), nil
	}

	source = strings.TrimPrefix(strings.TrimPrefix(source, "/"), "files/")
	storage := mediaSourceStorage(ctx, source)

	img, format, err := mediaLoadImage(ctx.DataDirOrDefault(), ctx.ProjectID, storage, source)
	if err != nil {
		return tools.FailErr(err), nil
	}

	// Calculate height maintaining aspect ratio if not specified.
	srcBounds := img.Bounds()
	if height <= 0 {
		height = int(float64(width) * float64(srcBounds.Dy()) / float64(srcBounds.Dx()))
		if height < 1 {
			height = 1
		}
	}

	if mediaDimsExceedCap(width, height) {
		return tools.Fail("output %dx%d is too large (max %d px per side, %d px total)", width, height, maxMediaSide, maxMediaOutputPixels), nil
	}

	resized := resizeImage(img, width, height)

	// Generate output filename.
	if output == "" {
		ext := filepath.Ext(source)
		name := strings.TrimSuffix(source, ext)
		output = fmt.Sprintf("%s_%dx%d%s", name, width, height, ext)
	}

	contentType := "image/jpeg"
	if format == "png" {
		contentType = "image/png"
	}

	_, size, err := mediaSaveImage(ctx.DataDirOrDefault(), ctx.ProjectID, storage, output, resized, format, 85)
	if err != nil {
		return tools.FailErr(err), nil
	}

	if err := mediaRegisterOutput(ctx, storage, output, contentType, size); err != nil {
		return tools.FailErr(err), nil
	}

	urlPrefix := "/files/"

	return tools.Ok(map[string]any{
		"filename": output,
		"width":    width,
		"height":   height,
		"size":     size,
		"url":      urlPrefix + output,
	}), nil
}

func mediaThumbnail(ctx *tools.ToolContext, source string, size int, output string) (*tools.Result, error) {
	if size <= 0 {
		size = 200
	}
	if mediaDimsExceedCap(size, size) {
		return tools.Fail("thumbnail size %d is too large (max %d px per side)", size, maxMediaSide), nil
	}
	source = strings.TrimPrefix(strings.TrimPrefix(source, "/"), "files/")
	storage := mediaSourceStorage(ctx, source)

	img, _, err := mediaLoadImage(ctx.DataDirOrDefault(), ctx.ProjectID, storage, source)
	if err != nil {
		return tools.FailErr(err), nil
	}

	thumb := cropCenter(img, size)

	ext := filepath.Ext(source)
	name := strings.TrimSuffix(source, ext)
	if output == "" {
		output = fmt.Sprintf("%s_thumb%s", name, ext)
	}

	format := "jpeg"
	contentType := "image/jpeg"
	if strings.ToLower(ext) == ".png" {
		format = "png"
		contentType = "image/png"
	}

	_, fileSize, err := mediaSaveImage(ctx.DataDirOrDefault(), ctx.ProjectID, storage, output, thumb, format, 85)
	if err != nil {
		return tools.FailErr(err), nil
	}

	if err := mediaRegisterOutput(ctx, storage, output, contentType, fileSize); err != nil {
		return tools.FailErr(err), nil
	}

	urlPrefix := "/files/"

	return tools.Ok(map[string]any{
		"filename": output,
		"size":     size,
		"bytes":    fileSize,
		"url":      urlPrefix + output,
	}), nil
}

func mediaOptimize(ctx *tools.ToolContext, source string, quality int) (*tools.Result, error) {
	source = strings.TrimPrefix(strings.TrimPrefix(source, "/"), "files/")
	storage := mediaSourceStorage(ctx, source)

	output := ""

	img, _, err := mediaLoadImage(ctx.DataDirOrDefault(), ctx.ProjectID, storage, source)
	if err != nil {
		return tools.FailErr(err), nil
	}

	// Always re-encode as JPEG for optimization regardless of input format.
	if output == "" {
		ext := filepath.Ext(source)
		name := strings.TrimSuffix(source, ext)
		output = name + "_opt.jpg"
	}

	_, fileSize, err := mediaSaveImage(ctx.DataDirOrDefault(), ctx.ProjectID, storage, output, img, "jpeg", quality)
	if err != nil {
		return tools.FailErr(err), nil
	}

	if err := mediaRegisterOutput(ctx, storage, output, "image/jpeg", fileSize); err != nil {
		return tools.FailErr(err), nil
	}

	urlPrefix := "/files/"

	return tools.Ok(map[string]any{
		"filename": output,
		"quality":  quality,
		"bytes":    fileSize,
		"url":      urlPrefix + output,
	}), nil
}

// mediaSourceStorage picks the storage dir a source image lives in. Uploads land in
// files/, while generated and stock images are written to assets/ (SaveBytesAsArtifact);
// both are served at /files/<name>, so the model names them the same way. Reading only
// files/ made every generated or stock image "file not found". Defaults to files.
func mediaSourceStorage(ctx *tools.ToolContext, filename string) string {
	if store.ValidateStorageFilename(filename) != nil {
		return "files"
	}
	for _, storage := range []string{"files", "assets"} {
		dir, _ := store.StorageDir(ctx.DataDirOrDefault(), ctx.ProjectID, mediaDirName(storage))
		if _, err := os.Stat(filepath.Join(dir, filename)); err == nil {
			return storage
		}
	}
	return "files"
}
