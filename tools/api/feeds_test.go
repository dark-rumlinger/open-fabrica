package api

import (
	"testing"

	"openfabrica/tools"
)

// TestFeedUpdatePatchesInPlace verifies update is a partial patch: an omitted
// option keeps its stored value instead of resetting, and a set option changes.
func TestFeedUpdatePatchesInPlace(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	if _, err := db.Exec(`CREATE TABLE posts (id INTEGER PRIMARY KEY, headline TEXT, body TEXT, created_at TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ctx := &tools.ToolContext{DB: db}

	if res, err := (&FeedCreate{
		EndpointPath: "blog", TableName: "posts", Format: "atom",
		Title: "Original", TitleColumn: "headline", BodyColumn: "body", ItemLimit: 25,
	}).Run(ctx); err != nil || res == nil || !res.Success {
		t.Fatalf("create feed: err=%v res=%+v", err, res)
	}

	// Patch only the title; every other option must be retained.
	if res, err := (&FeedUpdate{EndpointPath: "blog", Title: "Renamed"}).Run(ctx); err != nil || res == nil || !res.Success {
		t.Fatalf("update feed: err=%v res=%+v", err, res)
	}

	var format, title, titleCol string
	var itemLimit int
	if err := db.QueryRow(`SELECT format, title, title_column, item_limit FROM of_feed_endpoints WHERE path = ?`, "blog").
		Scan(&format, &title, &titleCol, &itemLimit); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if title != "Renamed" {
		t.Errorf("title should update to Renamed, got %q", title)
	}
	if format != "atom" || titleCol != "headline" || itemLimit != 25 {
		t.Errorf("omitted options should be retained, got format=%q title_column=%q item_limit=%d", format, titleCol, itemLimit)
	}

	// Updating a feed that does not exist fails clearly rather than creating one.
	if res, err := (&FeedUpdate{EndpointPath: "ghost", Title: "x"}).Run(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if res == nil || res.Success {
		t.Fatalf("expected failure updating a missing feed, got %+v", res)
	}
}
