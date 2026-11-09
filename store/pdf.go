package store

// Headless-Chrome print-to-PDF via CDP Page.printToPDF, layered on the shared
// LaunchBrowser launcher so both entry points inherit the same Chrome discovery
// and the same ErrBrowserUnavailable signal for callers to branch on.

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// pdfRenderTimeout bounds a single navigate+print cycle. Generous enough for a
// page that pulls a webfont or a chart library off a CDN before it settles,
// without letting one stuck document hold a Chrome process open indefinitely.
const pdfRenderTimeout = 30 * time.Second

// PDFOptions captures the page-layout knobs for a render. Zero values fall back
// to sensible print defaults (A4 portrait, backgrounds on, 0.4in margins) so a
// caller can pass an empty struct and still get a usable document.
type PDFOptions struct {
	// PageSize is a named paper size: "A4" (default), "Letter", or "Legal".
	PageSize string
	// Landscape rotates the page to landscape orientation.
	Landscape bool
	// PrintBackground includes background colours/images. Defaults to true;
	// set DisableBackground to turn it off (a bool default of false would
	// otherwise silently drop every styled background).
	DisableBackground bool
	// MarginInches is the uniform page margin in inches. <=0 uses 0.4in.
	MarginInches float64
	// DisableScripts turns off JavaScript in the rendered document. Set when the
	// HTML is filled from visitor-supplied data at request time.
	DisableScripts bool
}

// paperDimsInches returns the width and height in inches for the named size,
// defaulting to A4. Landscape rotation is applied by the caller via the CDP
// landscape flag, so the dimensions here are always portrait-oriented.
func paperDimsInches(size string) (w, h float64) {
	switch size {
	case "Letter", "letter":
		return 8.5, 11.0
	case "Legal", "legal":
		return 8.5, 14.0
	default: // A4
		return 8.27, 11.69
	}
}

// printActions builds the chromedp action that emits the PDF once the page is
// loaded, writing the bytes into *out. Shared by URL and HTML rendering so the
// two paths cannot drift on layout handling.
func printActions(opts PDFOptions, out *[]byte) chromedp.Action {
	w, h := paperDimsInches(opts.PageSize)
	margin := opts.MarginInches
	if margin <= 0 {
		margin = 0.4
	}
	return chromedp.ActionFunc(func(ctx context.Context) error {
		data, _, err := page.PrintToPDF().
			WithPrintBackground(!opts.DisableBackground).
			WithLandscape(opts.Landscape).
			WithPaperWidth(w).
			WithPaperHeight(h).
			WithMarginTop(margin).
			WithMarginBottom(margin).
			WithMarginLeft(margin).
			WithMarginRight(margin).
			WithPreferCSSPageSize(false).
			Do(ctx)
		if err != nil {
			return err
		}
		*out = data
		return nil
	})
}

// RenderURLToPDF loads a URL in a headless browser and returns its PDF bytes.
// cfg.HostResolverMap should be the project's vhost so a project-relative URL
// resolves to the local public server. Wraps ErrBrowserUnavailable when Chrome
// cannot launch.
func RenderURLToPDF(ctx context.Context, cfg BrowserConfig, url string, opts PDFOptions) ([]byte, error) {
	allocCtx, cancelAlloc, err := LaunchBrowser(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cancelAlloc()

	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()
	runCtx, cancelRun := context.WithTimeout(tabCtx, pdfRenderTimeout)
	defer cancelRun()

	var buf []byte
	if err := chromedp.Run(runCtx,
		chromedp.Navigate(url),
		// Let webfonts / late chart paints settle before capturing.
		chromedp.Sleep(400*time.Millisecond),
		printActions(opts, &buf),
	); err != nil {
		return nil, fmt.Errorf("rendering %s to PDF: %w", url, err)
	}
	return buf, nil
}

// RenderHTMLToPDF renders an inline HTML string to PDF without a project route.
// The document is loaded on about:blank, so relative asset URLs do NOT resolve
// to the project - inline styles or absolute URLs in the HTML, or use
// RenderURLToPDF against a real page when project CSS/assets are needed.
func RenderHTMLToPDF(ctx context.Context, cfg BrowserConfig, html string, opts PDFOptions) ([]byte, error) {
	allocCtx, cancelAlloc, err := LaunchBrowser(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cancelAlloc()

	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()
	runCtx, cancelRun := context.WithTimeout(tabCtx, pdfRenderTimeout)
	defer cancelRun()

	var buf []byte
	if err := chromedp.Run(runCtx,
		chromedp.Navigate("about:blank"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if opts.DisableScripts {
				return emulation.SetScriptExecutionDisabled(true).Do(ctx)
			}
			return nil
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			frameTree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return err
			}
			return page.SetDocumentContent(frameTree.Frame.ID, html).Do(ctx)
		}),
		chromedp.Sleep(400*time.Millisecond),
		printActions(opts, &buf),
	); err != nil {
		return nil, fmt.Errorf("rendering HTML to PDF: %w", err)
	}
	return buf, nil
}
