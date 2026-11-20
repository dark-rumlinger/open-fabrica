package probe

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"

	"openfabrica/internal/browser"
)

// PageScreenshot holds one rendered viewport capture of a project page.
type PageScreenshot struct {
	Path     string
	Viewport string // "desktop" (1280x800) or "mobile" (390x844)
	PNG      []byte
}

type captureViewport struct {
	name   string
	width  int64
	height int64
	mobile bool
}

var visualAuditViewports = []captureViewport{
	{name: "desktop", width: 1280, height: 800, mobile: false},
	{name: "mobile", width: 390, height: 844, mobile: true},
}

// CapturePageScreenshots renders up to maxPages project pages in headless Chrome
// (desktop and mobile viewports) and returns PNG screenshots for visual critique.
// Returns nil, nil when baseURL is empty, the project has no pages, or Chrome is
// unavailable.
func CapturePageScreenshots(ctx context.Context, db *sql.DB, baseURL, hostHeader string, maxPages int) ([]PageScreenshot, error) {
	if baseURL == "" || maxPages <= 0 {
		return nil, nil
	}
	pages := loadPagePaths(db)
	if len(pages) == 0 {
		return nil, nil
	}
	pages = prioritizeRootPages(pages, maxPages)

	cfg := browser.BrowserConfig{HostResolverMap: hostHeader}
	allocCtx, cancelAlloc, err := browser.LaunchBrowser(ctx, cfg)
	if err != nil {
		return nil, nil
	}
	allocCtx, cancelShared, err := startSharedFrom(allocCtx, cancelAlloc)
	if err != nil {
		return nil, nil
	}
	defer cancelShared()

	origin := baseURL
	if hostHeader != "" {
		origin = replaceHostInURL(baseURL, hostHeader)
	}
	authToken := authTokenFromContext(ctx)

	var out []PageScreenshot
	for _, p := range pages {
		if ctx.Err() != nil {
			return out, nil
		}
		concrete := concreteProbePath(db, p)
		targetURL := origin + concrete
		for _, vp := range visualAuditViewports {
			if ctx.Err() != nil {
				return out, nil
			}
			png, err := captureViewportPNG(allocCtx, targetURL, authToken, vp)
			if err != nil || len(png) == 0 {
				continue
			}
			out = append(out, PageScreenshot{
				Path:     p,
				Viewport: vp.name,
				PNG:      png,
			})
		}
	}
	return out, nil
}

func prioritizeRootPages(pages []string, maxPages int) []string {
	if len(pages) <= maxPages {
		return pages
	}
	out := make([]string, 0, maxPages)
	for _, p := range pages {
		if p == "/" {
			out = append(out, p)
			break
		}
	}
	for _, p := range pages {
		if len(out) >= maxPages {
			break
		}
		if p == "/" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func captureViewportPNG(parent context.Context, targetURL, authToken string, vp captureViewport) ([]byte, error) {
	tabCtx, cancelTab := browser.NewIsolatedTab(parent)
	defer cancelTab()
	runCtx, cancelTimeout := context.WithTimeout(tabCtx, 15*time.Second)
	defer cancelTimeout()

	var png []byte
	err := chromedp.Run(runCtx,
		emulation.SetDeviceMetricsOverride(vp.width, vp.height, 1, vp.mobile),
		browser.RuntimeInstrumentationAction(authToken),
		chromedp.Navigate(targetURL),
		chromedp.Sleep(browser.RuntimeIdleWindow),
		chromedp.CaptureScreenshot(&png),
	)
	if err != nil {
		return nil, fmt.Errorf("capture %s (%s): %w", targetURL, vp.name, err)
	}
	return png, nil
}
