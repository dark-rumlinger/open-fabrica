package probe

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// fakeEndpointServer routes the three non-WS realtime shapes by a path keyword so
// each smoke can be exercised against healthy and broken endpoints:
//   - a "good" path answers correctly; "boom" answers 500; "badtype" (SSE) answers
//     200 with the wrong content type.
func fakeEndpointServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/stream"):
			key := strings.TrimSuffix(strings.TrimPrefix(p, "/api/"), "/stream")
			switch key {
			case "boom":
				http.Error(w, "nope", http.StatusInternalServerError)
			case "badtype":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
			default:
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(": keepalive\n\n"))
			}
		case strings.HasPrefix(p, "/feed/"):
			key := strings.TrimPrefix(p, "/feed/")
			if key == "boom" {
				http.Error(w, "nope", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/rss+xml")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>x</title></channel></rss>`))
		case strings.HasSuffix(p, "/push/subscribe"):
			key := strings.TrimSuffix(strings.TrimPrefix(p, "/api/"), "/push/subscribe")
			if key == "boom" {
				http.Error(w, "nope", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, srv.URL
}

func memDB(t *testing.T, ddl ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range ddl {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("ddl %q: %v", s, err)
		}
	}
	return db
}

func sigSet(issues []ValidationIssue) map[string]bool {
	out := map[string]bool{}
	for _, i := range issues {
		out[i.Signature] = true
	}
	return out
}

func TestRunSSESmoke(t *testing.T) {
	_, base := fakeEndpointServer(t)
	db := memDB(t,
		`CREATE TABLE of_stream_endpoints (path TEXT, requires_auth INTEGER)`,
		`INSERT INTO of_stream_endpoints (path, requires_auth) VALUES ('good',0),('badtype',0),('boom',0)`,
	)
	issues := RunSSESmoke(context.Background(), db, base, "")
	sigs := sigSet(issues)
	if len(issues) != 2 || !sigs["sse_status"] || !sigs["sse_contenttype"] {
		t.Fatalf("want sse_status + sse_contenttype (good passes), got %+v", issues)
	}
}

func TestRunFeedSmoke(t *testing.T) {
	_, base := fakeEndpointServer(t)
	db := memDB(t,
		`CREATE TABLE of_feed_endpoints (path TEXT)`,
		`INSERT INTO of_feed_endpoints (path) VALUES ('good'),('boom')`,
	)
	issues := RunFeedSmoke(context.Background(), db, base, "")
	if len(issues) != 1 || issues[0].Signature != "feed_status" {
		t.Fatalf("want one feed_status issue (good renders XML), got %+v", issues)
	}
}

func TestRunPushSmoke(t *testing.T) {
	_, base := fakeEndpointServer(t)
	db := memDB(t,
		`CREATE TABLE of_push_endpoints (path TEXT, requires_auth INTEGER)`,
		`CREATE TABLE of_push_subscriptions (endpoint_url TEXT)`,
		`INSERT INTO of_push_endpoints (path, requires_auth) VALUES ('good',0),('boom',0)`,
		// Pre-seed the row the "good" probe will create, to prove cleanup removes it.
		`INSERT INTO of_push_subscriptions (endpoint_url) VALUES ('https://probe.invalid/of-probe-good')`,
	)
	issues := RunPushSmoke(context.Background(), db, base, "")
	if len(issues) != 1 || issues[0].Signature != "push_subscribe" {
		t.Fatalf("want one push_subscribe issue (good is accepted), got %+v", issues)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM of_push_subscriptions WHERE endpoint_url = 'https://probe.invalid/of-probe-good'`).Scan(&n)
	if n != 0 {
		t.Errorf("push smoke must delete its probe subscription, still present (%d rows)", n)
	}
}

// Auth-gated endpoints with no probe token are skipped, not reported as failures.
func TestRunSSESmoke_SkipsAuthWithoutToken(t *testing.T) {
	_, base := fakeEndpointServer(t)
	db := memDB(t,
		`CREATE TABLE of_stream_endpoints (path TEXT, requires_auth INTEGER)`,
		`INSERT INTO of_stream_endpoints (path, requires_auth) VALUES ('boom',1)`,
	)
	if issues := RunSSESmoke(context.Background(), db, base, ""); len(issues) != 0 {
		t.Fatalf("an auth-gated SSE endpoint with no token must be skipped, got %+v", issues)
	}
}
