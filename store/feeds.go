package store

import (
	"database/sql"
	"strings"
)

// FeedEndpoint holds a row from of_feed_endpoints.
type FeedEndpoint struct {
	TableName   string
	Format      string
	Title       string
	Description string
	Link        string
	TitleColumn string
	BodyColumn  string
	LinkColumn  string
	DateColumn  string
	ItemLimit   int
	WhereJSON   string
}

// FeedItemRow holds a single scanned item row for RSS/Atom rendering.
type FeedItemRow struct {
	Title string
	Body  string
	Date  string
	Link  string
}

// LoadFeedEndpoint loads a feed definition by path from of_feed_endpoints.
func LoadFeedEndpoint(reader *sql.DB, path string) (*FeedEndpoint, error) {
	var ep FeedEndpoint
	var linkColNull sql.NullString
	err := reader.QueryRow(`
		SELECT table_name, format, title, description, link,
		       title_column, body_column, link_column, date_column,
		       item_limit, where_json
		FROM of_feed_endpoints WHERE path = ?`, path,
	).Scan(&ep.TableName, &ep.Format, &ep.Title, &ep.Description, &ep.Link,
		&ep.TitleColumn, &ep.BodyColumn, &linkColNull, &ep.DateColumn,
		&ep.ItemLimit, &ep.WhereJSON)
	if err != nil {
		return nil, err
	}
	ep.LinkColumn = linkColNull.String
	return &ep, nil
}

// LoadFeedItems queries the bound table for a feed endpoint and returns the raw item rows.
func LoadFeedItems(reader *sql.DB, ep *FeedEndpoint, whereSQL string, whereParams []any) ([]FeedItemRow, error) {
	cols := []string{ep.TitleColumn, ep.BodyColumn, ep.DateColumn}
	if ep.LinkColumn != "" {
		cols = append(cols, ep.LinkColumn)
	}
	query := "SELECT " + strings.Join(cols, ", ") + " FROM " + ep.TableName + whereSQL +
		" ORDER BY " + ep.DateColumn + " DESC LIMIT ?"
	queryArgs := append(append([]any{}, whereParams...), ep.ItemLimit)
	rows, err := reader.Query(query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []FeedItemRow{}
	for rows.Next() {
		var it FeedItemRow
		var linkVal sql.NullString
		var scanErr error
		if ep.LinkColumn != "" {
			scanErr = rows.Scan(&it.Title, &it.Body, &it.Date, &linkVal)
		} else {
			scanErr = rows.Scan(&it.Title, &it.Body, &it.Date)
		}
		if scanErr != nil {
			continue
		}
		if linkVal.Valid {
			it.Link = linkVal.String
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
