package public

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openfabrica/security"
	"openfabrica/store"
)

// Feed handles GET /feed/<path>. Reads the feed definition from
// of_feed_endpoints and renders RSS 2.0 or Atom 1.0 from the bound table.
func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	projectDB := h.openProjectDB(w, project.ID)
	if projectDB == nil {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/feed/")
	path = strings.TrimSuffix(path, ".xml")
	if path == "" {
		writeError(w, http.StatusBadRequest, "feed path required")
		return
	}

	ep, err := store.LoadFeedEndpoint(projectDB.Reader(), path)
	if err != nil {
		writeError(w, http.StatusNotFound, "feed not found")
		return
	}

	if security.ValidateTableName(ep.TableName) != nil ||
		security.ValidateColumnName(ep.TitleColumn) != nil ||
		security.ValidateColumnName(ep.BodyColumn) != nil ||
		security.ValidateColumnName(ep.DateColumn) != nil {
		writeError(w, http.StatusBadRequest, "invalid feed column")
		return
	}
	if ep.LinkColumn != "" && security.ValidateColumnName(ep.LinkColumn) != nil {
		writeError(w, http.StatusBadRequest, "invalid link column")
		return
	}

	// Build the parameterized WHERE clause from the stored structured filter.
	var whereSQL string
	var whereParams []any
	if ep.WhereJSON != "" && ep.WhereJSON != "[]" {
		var filters []any
		if jsonErr := json.Unmarshal([]byte(ep.WhereJSON), &filters); jsonErr != nil {
			writeError(w, http.StatusInternalServerError, "feed filter parse: "+jsonErr.Error())
			return
		}
		clause, params, wErr := store.BuildWhereClause(filters)
		if wErr != nil {
			writeError(w, http.StatusBadRequest, "feed filter: "+wErr.Error())
			return
		}
		whereSQL = clause
		whereParams = params
	}

	rawItems, err := store.LoadFeedItems(projectDB.Reader(), ep, whereSQL, whereParams)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "feed query: "+err.Error())
		return
	}

	items := make([]feedItemAlias, 0, len(rawItems))
	for _, r := range rawItems {
		items = append(items, feedItemAlias{
			Title: r.Title,
			Body:  r.Body,
			Date:  parseFeedDate(r.Date),
			Link:  r.Link,
		})
	}

	format, title, desc, link := ep.Format, ep.Title, ep.Description, ep.Link

	if format == "atom" {
		renderAtom(w, title, desc, link, items)
		return
	}
	renderRSS(w, title, desc, link, items)
}

func parseFeedDate(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Now().UTC()
}

type rssDocument struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel rssChannel
}

type rssChannel struct {
	XMLName     xml.Name  `xml:"channel"`
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Items       []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link,omitempty"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	GUID        string `xml:"guid"`
}

func renderRSS(w http.ResponseWriter, title, desc, link string, items []feedItemAlias) {
	channel := rssChannel{Title: title, Link: link, Description: desc}
	for _, it := range items {
		channel.Items = append(channel.Items, rssItem{
			Title:       it.Title,
			Link:        it.Link,
			Description: it.Body,
			PubDate:     it.Date.UTC().Format(time.RFC1123Z),
			GUID:        fmt.Sprintf("%s#%d", link, it.Date.UnixNano()),
		})
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(rssDocument{Version: "2.0", Channel: channel})
}

type atomDocument struct {
	XMLName  xml.Name `xml:"feed"`
	Xmlns    string   `xml:"xmlns,attr"`
	Title    string   `xml:"title"`
	ID       string   `xml:"id"`
	Updated  string   `xml:"updated"`
	Subtitle string   `xml:"subtitle,omitempty"`
	Link     atomLink `xml:"link"`
	Entries  []atomEntry
}
type atomLink struct {
	Href string `xml:"href,attr"`
}
type atomEntry struct {
	XMLName xml.Name `xml:"entry"`
	Title   string   `xml:"title"`
	ID      string   `xml:"id"`
	Updated string   `xml:"updated"`
	Content struct {
		Type    string `xml:"type,attr"`
		Content string `xml:",chardata"`
	} `xml:"content"`
	Link atomLink `xml:"link"`
}

func renderAtom(w http.ResponseWriter, title, desc, link string, items []feedItemAlias) {
	doc := atomDocument{
		Xmlns:    "http://www.w3.org/2005/Atom",
		Title:    title,
		ID:       link,
		Updated:  time.Now().UTC().Format(time.RFC3339),
		Subtitle: desc,
		Link:     atomLink{Href: link},
	}
	for _, it := range items {
		entry := atomEntry{
			Title:   it.Title,
			ID:      fmt.Sprintf("%s#%d", link, it.Date.UnixNano()),
			Updated: it.Date.UTC().Format(time.RFC3339),
			Link:    atomLink{Href: it.Link},
		}
		entry.Content.Type = "html"
		entry.Content.Content = it.Body
		doc.Entries = append(doc.Entries, entry)
	}
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(doc)
}

// feedItemAlias is a local alias so the local `item` struct can be shared
// between RSS and Atom renderers without exporting it.
type feedItemAlias = struct {
	Title string
	Body  string
	Date  time.Time
	Link  string
}
