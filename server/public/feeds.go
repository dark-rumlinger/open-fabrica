package public

import (
	"database/sql"
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

	var (
		table, format, title, desc, link string
		titleCol, bodyCol, dateCol       string
		whereJSON                        string
		linkColNull                      sql.NullString
		itemLimit                        int
	)
	err := projectDB.Reader().QueryRow(`
		SELECT table_name, format, title, description, link,
		       title_column, body_column, link_column, date_column,
		       item_limit, where_json
		FROM of_feed_endpoints WHERE path = ?`, path,
	).Scan(&table, &format, &title, &desc, &link,
		&titleCol, &bodyCol, &linkColNull, &dateCol, &itemLimit, &whereJSON)
	if err != nil {
		writeError(w, http.StatusNotFound, "feed not found")
		return
	}

	if security.ValidateTableName(table) != nil ||
		security.ValidateColumnName(titleCol) != nil ||
		security.ValidateColumnName(bodyCol) != nil ||
		security.ValidateColumnName(dateCol) != nil {
		writeError(w, http.StatusBadRequest, "invalid feed column")
		return
	}
	linkCol := linkColNull.String
	if linkCol != "" && security.ValidateColumnName(linkCol) != nil {
		writeError(w, http.StatusBadRequest, "invalid link column")
		return
	}

	// Build the parameterized WHERE clause from the stored structured filter.
	var whereSQL string
	var whereParams []any
	if whereJSON != "" && whereJSON != "[]" {
		var filters []any
		if jsonErr := json.Unmarshal([]byte(whereJSON), &filters); jsonErr != nil {
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

	cols := titleCol + ", " + bodyCol + ", " + dateCol
	if linkCol != "" {
		cols = titleCol + ", " + bodyCol + ", " + dateCol + ", " + linkCol
	}
	query := "SELECT " + cols + " FROM " + table + whereSQL +
		" ORDER BY " + dateCol + " DESC LIMIT ?"
	queryArgs := append(whereParams, itemLimit)
	rows, err := projectDB.Reader().Query(query, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "feed query: "+err.Error())
		return
	}
	defer rows.Close()

	items := []feedItemAlias{}
	for rows.Next() {
		var it feedItemAlias
		var dateStr string
		var linkVal sql.NullString
		var scanErr error
		if linkCol != "" {
			scanErr = rows.Scan(&it.Title, &it.Body, &dateStr, &linkVal)
		} else {
			scanErr = rows.Scan(&it.Title, &it.Body, &dateStr)
		}
		if scanErr != nil {
			continue
		}
		it.Date = parseFeedDate(dateStr)
		if linkVal.Valid {
			it.Link = linkVal.String
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "feed rows: "+err.Error())
		return
	}

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
