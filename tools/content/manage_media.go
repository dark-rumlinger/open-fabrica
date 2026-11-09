package content

import "openfabrica/tools"

// manage_media is the image transformation surface: resize, thumbnail,
// re-encode. Operates on artifacts already stored in /files.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_media",
		Category: "content",
		OneLiner: "Transform stored images - resize, thumbnail, or optimize an existing asset.",
		// Gated on image_source: every input it accepts comes from generation or a
		// stock fetch, so without either provider there is nothing for it to
		// transform and offering it only invites a search for the missing producer.
		RequiresCapability: "image_source",
		WhenToUse: `Use after an image lands in /files (manage_image_gen, an upload, or a stock
photo fetch) and you need a different size for a hero, thumbnail grid, or
faster page load. Output is a NEW artifact beside the original (suffixed _WxH, _thumb, or _opt.jpg); the original is kept, so point pages at the path the result returns.`,
		Actions: []tools.Action{
			&MediaResize{}, &MediaThumbnail{}, &MediaOptimize{},
		},
	})
}

type MediaResize struct {
	Filename string `arg:"filename,required" desc:"Existing image artifact."`
	Width    int    `arg:"width,required"    desc:"Output width in pixels."`
	Height   int    `arg:"height"            desc:"Output height in pixels; omit to keep the aspect ratio."`
	OutPath  string `arg:"out_path"          desc:"Override the destination path."`
}

func (a *MediaResize) Doc() string { return "Resize an image to width/height." }
func (a *MediaResize) Example() any {
	return &MediaResize{Filename: "hero.jpg", Width: 1920, Height: 1080}
}
func (a *MediaResize) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return mediaResize(ctx, a.Filename, a.Width, a.Height, a.OutPath)
}

type MediaThumbnail struct {
	Filename string `arg:"filename,required" desc:"Existing image artifact."`
	Size     int    `arg:"size"              desc:"Output side length in pixels (square). Default 200 if omitted." default:"200"`
	OutPath  string `arg:"out_path"          desc:"Override the destination path."`
}

func (a *MediaThumbnail) Doc() string { return "Square-crop an image at a chosen size." }
func (a *MediaThumbnail) Example() any {
	return &MediaThumbnail{Filename: "hero.jpg", Size: 320}
}
func (a *MediaThumbnail) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return mediaThumbnail(ctx, a.Filename, a.Size, a.OutPath)
}

type MediaOptimize struct {
	Filename string `arg:"filename,required" desc:"Existing image artifact."`
	Quality  int    `arg:"quality"           desc:"JPEG quality 1-100." default:"80" min:"1" max:"100"`
}

func (a *MediaOptimize) Doc() string { return "Re-encode as optimized JPEG." }
func (a *MediaOptimize) Example() any {
	return &MediaOptimize{Filename: "screenshot.png", Quality: 80}
}
func (a *MediaOptimize) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return mediaOptimize(ctx, a.Filename, a.Quality)
}
