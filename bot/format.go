package bot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"openfabrica/db/models"
	"openfabrica/tlsproxy"
)

// The builder writes its messages in markdown, with a machine-readable payload
// hidden in an HTML comment at the end of the go-live message. Telegram shows
// neither correctly, so messages are converted before they are sent.

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	fenceRe       = regexp.MustCompile("(?s)```[A-Za-z0-9_+-]*\n?(.*?)```")
	inlineCodeRe  = regexp.MustCompile("`([^`\n]+)`")
	boldRe        = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	linkRe        = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)"]+)\)`)
	headingRe     = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*$`)
	bulletRe      = regexp.MustCompile(`(?m)^([ \t]*)[-*][ \t]+`)
	blankRunsRe   = regexp.MustCompile(`\n{3,}`)
	// Any link left after the http(s) ones were converted: keep just the words.
	// One level of nested parentheses is tolerated, as in javascript:alert(1).
	relativeLinkRe = regexp.MustCompile(`\[([^\]\n]+)\]\((?:[^()\s]|\([^()\s]*\))*\)`)
)

// mdToTelegramHTML turns the builder's markdown into Telegram's HTML subset. It
// escapes first, so model output can never inject markup, then converts the few
// constructs the builder uses: code, bold, links, headings and bullets. Relative
// links (the admin panel's own routes) keep their text and drop the link.
func mdToTelegramHTML(md string) string {
	s := htmlCommentRe.ReplaceAllString(md, "")
	s = escapeHTML(s)

	// Park code so nothing inside it is reformatted.
	var parked []string
	park := func(html string) string {
		parked = append(parked, html)
		return fmt.Sprintf("\x00%d\x00", len(parked)-1)
	}
	s = fenceRe.ReplaceAllStringFunc(s, func(m string) string {
		return park("<pre>" + strings.Trim(fenceRe.FindStringSubmatch(m)[1], "\n") + "</pre>")
	})
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		return park("<code>" + inlineCodeRe.FindStringSubmatch(m)[1] + "</code>")
	})

	s = headingRe.ReplaceAllString(s, "<b>$1</b>")
	s = boldRe.ReplaceAllString(s, "<b>$1</b>")
	s = linkRe.ReplaceAllString(s, `<a href="$2">$1</a>`)
	s = bulletRe.ReplaceAllString(s, "$1• ")
	// [text](/relative) and the like: keep the words.
	s = relativeLinkRe.ReplaceAllString(s, "$1")

	for i, html := range parked {
		s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), html, 1)
	}
	return strings.TrimSpace(blankRunsRe.ReplaceAllString(s, "\n\n"))
}

// --- go-live message ---

var goLiveMarkerRe = regexp.MustCompile(`(?s)<!--of-golive:(.*?)-->`)

// goLiveInfo mirrors the structured payload the engine appends to its go-live
// message (engine/live_summary.go goLivePayload).
type goLiveInfo struct {
	KnownIssues bool     `json:"known_issues"`
	PreviewURL  string   `json:"preview_url"`
	Pages       []string `json:"pages"`
	Tables      []string `json:"tables"`
	APIs        []string `json:"apis"`
	// FlowUnverified names the user flows that did not pass.
	FlowUnverified []string `json:"flow_unverified"`
	Login          *struct {
		LoginURL   string `json:"login_url"`
		Label      string `json:"label"`
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
		Minted     bool   `json:"minted"`
	} `json:"login"`
}

// parseGoLive extracts the structured go-live payload from a builder message.
func parseGoLive(content string) (*goLiveInfo, bool) {
	m := goLiveMarkerRe.FindStringSubmatch(content)
	if m == nil {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m[1]))
	if err != nil {
		return nil, false
	}
	var info goLiveInfo
	if json.Unmarshal(raw, &info) != nil {
		return nil, false
	}
	return &info, true
}

// publicSiteURL is the address a phone can open, or "" when the project has no
// real public domain (a .localhost address only works on the host computer).
func (s *BotService) publicSiteURL(p *models.Project) string {
	if p.Domain == nil || s.deps.TLSDisabled {
		return ""
	}
	if tlsproxy.DomainKind(*p.Domain) != tlsproxy.DomainKindPublic {
		return ""
	}
	return "https://" + *p.Domain
}

func nameList(v voice, items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return v.tr("%s +%d more", strings.Join(items[:max], ", "), len(items)-max)
}

// goLiveCard renders the "your project is live" message for a phone: what was
// built, the demo sign-in, and a link only when one would actually open.
func (s *BotService) goLiveCard(v voice, project *models.Project, info *goLiveInfo) screen {
	var b strings.Builder
	if info.KnownIssues {
		b.WriteString(v.tr("⚠️ <b>Deployed, with known issues</b>") + "\n")
	} else {
		b.WriteString(v.tr("🎉 <b>Your project is live!</b>") + "\n")
	}
	fmt.Fprintf(&b, "%s\n\n", escapeHTML(project.Name))

	if len(info.Pages) > 0 {
		b.WriteString(v.trh("📄 Pages: %s", nameList(v, info.Pages, 6)) + "\n")
	}
	if len(info.Tables) > 0 {
		b.WriteString(v.trh("🗄 Data: %s", nameList(v, info.Tables, 6)) + "\n")
	}
	if len(info.APIs) > 0 {
		b.WriteString(v.trh("🔌 APIs: %s", nameList(v, info.APIs, 6)) + "\n")
	}
	if len(info.FlowUnverified) > 0 {
		b.WriteString(v.trh("🧪 Flow tests that did not pass: %s", nameList(v, info.FlowUnverified, 4)) + "\n")
	}
	if l := info.Login; l != nil && l.Minted && l.Identifier != "" {
		label := l.Label
		if label == "" {
			label = v.tr("Login")
		}
		b.WriteString("\n" + v.tr("🔑 Demo sign-in") + "\n" +
			v.trh("%s: <code>%s</code>", label, l.Identifier) + "\n" +
			v.trh("Password: <code>%s</code>", l.Password) + "\n")
	}

	labels := []string{}
	cbs := []string{}
	if url := s.publicSiteURL(project); url != "" {
		fmt.Fprintf(&b, "\n%s\n", escapeHTML(url))
		labels = append(labels, v.tr("🔗 Open site"))
		cbs = append(cbs, "url:"+url)
	} else {
		b.WriteString("\n" + v.tr("This address only works on the computer running Open Fabrica. Set a real domain to open it from your phone.") + "\n")
	}
	b.WriteString("\n" + v.tr("Chat here anytime to ask questions or request changes."))

	labels = append(labels, v.tr("🌐 Site"), v.tr("📊 Status"), v.tr("☰ Menu"))
	cbs = append(cbs, "nav:site", "nav:status", "nav:menu")
	return screen{text: b.String(), labels: labels, cbs: cbs}
}
