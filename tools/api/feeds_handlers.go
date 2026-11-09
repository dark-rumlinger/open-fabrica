package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"openfabrica/store"
	"openfabrica/tools"
)

// manage_feeds: register RSS / Atom feed endpoints that render rows from a
// table. The actual feed bytes are emitted by server/public/feeds.go on
// GET /feed/<path>.

// feedCreate takes a fully-resolved FeedCreate config and writes it as a
// FULL-ROW upsert: every column is rewritten from this call's config. create
// calls it directly; update (feedUpdate) loads the stored row, overlays the
// fields this call set, and replays the merged config through here.
func feedCreate(ctx *tools.ToolContext, a *FeedCreate) (*tools.Result, error) {
	// Unpack the config into the local names the handler body uses.
	rawPath := a.EndpointPath
	table := a.TableName
	formatArg := a.Format
	title := a.Title
	desc := a.Description
	linkArg := a.Link
	titleColArg := a.TitleColumn
	bodyColArg := a.BodyColumn
	linkCol := a.LinkColumn
	dateColArg := a.DateColumn
	itemLimit := a.ItemLimit
	whereRaw := a.Where

	path, errRes := tools.ValidateAndNormalizeEndpointPath(rawPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" || table == "" {
		return tools.Fail("endpoint_path and table_name are required"), nil
	}
	format := formatArg
	if format == "" {
		format = "rss"
	}
	titleCol := titleColArg
	if titleCol == "" {
		titleCol = "title"
	}
	bodyCol := bodyColArg
	if bodyCol == "" {
		bodyCol = "body"
	}
	dateCol := dateColArg
	if dateCol == "" {
		dateCol = "created_at"
	}
	// Validate column identifiers up-front so a malformed feed never reaches
	// the public render path (which rejects them and returns 500). Catches the
	// typo at BUILD time instead of at first reader.
	for _, c := range []struct{ name, val string }{
		{"title_column", titleCol},
		{"body_column", bodyCol},
		{"date_column", dateCol},
	} {
		if !store.ValidColumnName.MatchString(c.val) {
			return tools.Fail("invalid %s: %q", c.name, c.val), nil
		}
	}
	if linkCol != "" && !store.ValidColumnName.MatchString(linkCol) {
		return tools.Fail("invalid link_column: %q", linkCol), nil
	}
	// The columns must exist on the table, or the feed renders empty items at read
	// time; checked here so the miss names the table's real columns.
	if cols := feedTableColumns(ctx, table); cols != nil {
		for _, c := range []struct{ name, val string }{
			{"title_column", titleCol}, {"body_column", bodyCol}, {"date_column", dateCol}, {"link_column", linkCol},
		} {
			if c.val != "" && !cols[c.val] {
				return tools.FailCode(tools.CodeBadArg, "%s %q is not a column of %s (columns: %s)", c.name, c.val, table, strings.Join(sortedKeys(cols), ", ")), nil
			}
		}
	} else {
		return tools.FailCode(tools.CodeNotFound, "table %q not found - create it first with manage_schema create_table", table), nil
	}
	link := linkArg
	if link == "" {
		link = "/"
	}
	limit := 50
	if itemLimit > 0 {
		limit = itemLimit
	}

	// Validate the structured filter at create time so the LLM gets a clear
	// error instead of a runtime "feed query failed" at /feed read time.
	whereJSON := "[]"
	if len(whereRaw) > 0 {
		if _, _, err := store.BuildWhereClause(whereRaw); err != nil {
			return tools.Fail("%s", "invalid where filter: "+err.Error()), nil
		}
		b, _ := json.Marshal(whereRaw)
		whereJSON = string(b)
	}

	_, err := ctx.DB.Exec(`
		INSERT INTO of_feed_endpoints
		  (path, table_name, format, title, description, link, title_column, body_column, link_column, date_column, item_limit, where_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  table_name = excluded.table_name,
		  format = excluded.format,
		  title = excluded.title,
		  description = excluded.description,
		  link = excluded.link,
		  title_column = excluded.title_column,
		  body_column = excluded.body_column,
		  link_column = excluded.link_column,
		  date_column = excluded.date_column,
		  item_limit = excluded.item_limit,
		  where_json = excluded.where_json`,
		path, table, format, title, desc, link, titleCol, bodyCol, tools.NullIfEmpty(linkCol), dateCol, limit, whereJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("creating feed: %w", err)
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "url": "/feed/" + path}), nil
}

// loadFeedConfig reads a stored feed endpoint into a FeedCreate config, or
// returns an error if no feed owns that path.
func loadFeedConfig(ctx *tools.ToolContext, path string) (*FeedCreate, error) {
	var c FeedCreate
	var linkCol *string
	var whereJSON string
	err := ctx.DB.QueryRow(`SELECT table_name, format, title, description, link, title_column, body_column, link_column, date_column, item_limit, where_json FROM of_feed_endpoints WHERE path = ?`, path).
		Scan(&c.TableName, &c.Format, &c.Title, &c.Description, &c.Link, &c.TitleColumn, &c.BodyColumn, &linkCol, &c.DateColumn, &c.ItemLimit, &whereJSON)
	if err != nil {
		return nil, err
	}
	c.EndpointPath = path
	if linkCol != nil {
		c.LinkColumn = *linkCol
	}
	if whereJSON != "" && whereJSON != "[]" {
		_ = json.Unmarshal([]byte(whereJSON), &c.Where)
	}
	return &c, nil
}

// feedUpdate patches a stored feed: it loads the current config and overlays
// only the fields this call set, so an omitted option keeps its stored value
// instead of resetting. A zero value (empty string, 0) reads as "keep": clearing
// a feed's title or a column to empty is never the intent, so the safe default
// is to leave it. The merged config is written through the full-row upsert.
func feedUpdate(ctx *tools.ToolContext, a *FeedUpdate) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	cur, err := loadFeedConfig(ctx, path)
	if err != nil {
		return tools.Fail("no feed at /feed/%s to update - create it first with action=create", path), nil
	}
	if a.TableName != "" {
		cur.TableName = a.TableName
	}
	if a.Format != "" {
		cur.Format = a.Format
	}
	if a.Title != "" {
		cur.Title = a.Title
	}
	if a.Description != "" {
		cur.Description = a.Description
	}
	if a.Link != "" {
		cur.Link = a.Link
	}
	if a.TitleColumn != "" {
		cur.TitleColumn = a.TitleColumn
	}
	if a.BodyColumn != "" {
		cur.BodyColumn = a.BodyColumn
	}
	if a.LinkColumn != "" {
		cur.LinkColumn = a.LinkColumn
	}
	if a.DateColumn != "" {
		cur.DateColumn = a.DateColumn
	}
	if a.ItemLimit > 0 {
		cur.ItemLimit = a.ItemLimit
	}
	if len(a.Where) > 0 {
		cur.Where = a.Where
	}
	return feedCreate(ctx, cur)
}

func feedList(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(`SELECT id, path, table_name, format, title, item_limit, created_at FROM of_feed_endpoints ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, limit int
		var path, table, format, title string
		var created time.Time
		if err := rows.Scan(&id, &path, &table, &format, &title, &limit, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "endpoint_path": path, "table_name": table, "format": format,
			"title": title, "item_limit": limit, "url": "/feed/" + path, "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tools.Ok(out), nil
}

func feedDelete(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_feed_endpoints", "feed", "manage_feeds", "delete", endpointPath)
}

// feedTableColumns returns the live column set of a project table, or nil when the
// table does not exist.
func feedTableColumns(ctx *tools.ToolContext, tableName string) map[string]bool {
	physical, err := store.SanitizedTableName(tableName)
	if err != nil {
		return nil
	}
	rows, err := ctx.DB.Query(fmt.Sprintf("PRAGMA table_info(%s)", physical))
	if err != nil {
		return nil
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk) == nil {
			cols[name] = true
		}
	}
	if rows.Err() != nil {
		return nil
	}
	if len(cols) == 0 {
		return nil
	}
	return cols
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
