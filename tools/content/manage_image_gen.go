package content

import "openfabrica/tools"

// manage_image_gen generates AI images from a prompt. Outputs are saved as
// kind=file, scope=global. Provider gated by the "image" capability.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_image_gen",
		Category:           "content",
		RequiresCapability: "image",
		OneLiner:           "Generate AI images from a prompt (saved as kind=file, scope=global).",
		WhenToUse: `Use when the plan needs marketing images, illustrations, or hero photography.
Outputs land under /files/<filename> and are referenced by pages via standard
<img> tags. Re-runs replace previous artifacts at the same path. Use
list_providers / list_models to see configured image backends before generating.`,
		Actions: []tools.Action{
			&ImageGenerate{}, &ImageListProviders{}, &ImageListModels{},
		},
	})
}

type ImageGenerate struct {
	Prompt   string `arg:"prompt,required" desc:"Text prompt describing the image."`
	Size     string `arg:"size"            desc:"Pixel size, e.g. '1024x1024'." default:"1024x1024"`
	Alt      string `arg:"alt"             desc:"Alt-text saved on the artifact."`
	Filename string `arg:"filename"        desc:"Name prefix instead of the prompt-derived one: images save as <filename>-<n>.<ext> (the extension follows the image format; any you pass is dropped). Link the path the result returns."`
	Provider string `arg:"provider"        desc:"Which configured image provider to use."`
	Model    string `arg:"model"           desc:"Model id on the chosen provider."`
	Count    int    `arg:"count"           desc:"Number of images to produce." default:"1"`
}

func (a *ImageGenerate) ActionName() string { return "generate" }
func (a *ImageGenerate) Doc() string        { return "Produce one or more images from a prompt." }
func (a *ImageGenerate) Example() any {
	return &ImageGenerate{Prompt: "warm sourdough loaf on linen", Size: "1024x1024", Alt: "Sourdough loaf"}
}
func (a *ImageGenerate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return imageGenerate(ctx, a.Prompt, a.Size, a.Alt, a.Filename, a.Provider, a.Model, a.Count)
}

type ImageListProviders struct{}

func (a *ImageListProviders) ActionName() string { return "list_providers" }
func (a *ImageListProviders) Doc() string        { return "List configured image providers." }
func (a *ImageListProviders) ReadOnly() bool     { return true }
func (a *ImageListProviders) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return imageListProviders(ctx)
}

type ImageListModels struct {
	Provider string `arg:"provider" desc:"Provider name; omit for the default image provider."`
}

func (a *ImageListModels) ActionName() string { return "list_models" }
func (a *ImageListModels) Doc() string        { return "List models available on one provider." }
func (a *ImageListModels) ReadOnly() bool     { return true }
func (a *ImageListModels) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return imageListModels(ctx, a.Provider)
}
