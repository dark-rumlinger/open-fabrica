package flowsnap

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
)

// FlowBrowserMarker is appended to the user agent of flow sessions that override
// it (mobile emulation), so their page views are never mistaken for real visitors.
// Headless Chrome's own user agent already says "HeadlessChrome".
const FlowBrowserMarker = "OFFlowTest"

// trafficWindow is how far back real visitors block a replay: a restore would
// revert anything they wrote while it ran.
const trafficWindow = 10 * time.Minute

// IsLocalDomain reports whether a project domain is only reachable from this
// machine (localhost, *.localhost, an IP or none), so no outside visitor can be
// using the app while a replay runs.
func IsLocalDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" || d == "localhost" || strings.HasSuffix(d, ".localhost") {
		return true
	}
	return net.ParseIP(d) != nil
}

// RecentRealTraffic reports whether real visitors (not headless or flow-test
// browsers) viewed the app within the window.
func RecentRealTraffic(pdb *sql.DB, window time.Duration) (bool, error) {
	since := time.Now().UTC().Add(-window).Format("2006-01-02 15:04:05")
	var n int
	err := pdb.QueryRow(`SELECT COUNT(*) FROM of_analytics
		WHERE created_at >= ?
		  AND COALESCE(user_agent, '') NOT LIKE '%HeadlessChrome%'
		  AND COALESCE(user_agent, '') NOT LIKE ?`, since, "%"+FlowBrowserMarker+"%").Scan(&n)
	return n > 0, err
}

// Opener prepares a replay: it returns a snapshot to Restore afterwards, or a
// non-empty skip reason when the replay must not run.
type Opener func(ctx context.Context) (snap *Snapshot, skip string, err error)

type openerKey struct{}

// WithOpener attaches an opener for one project to ctx. The engine puts one on the
// context of everything that replays flows (validators and tools).
func WithOpener(ctx context.Context, globalDB *sql.DB, pdb *db.ProjectDB, projectDir string, projectID int) context.Context {
	if globalDB == nil || pdb == nil {
		return ctx
	}
	return WithOpenerFunc(ctx, func(ctx context.Context) (*Snapshot, string, error) {
		return Open(ctx, globalDB, pdb, projectDir, projectID)
	})
}

// WithOpenerFunc attaches an arbitrary opener (tests).
func WithOpenerFunc(ctx context.Context, open Opener) context.Context {
	return context.WithValue(ctx, openerKey{}, open)
}

// OpenerFrom returns the opener on ctx, or nil when none was attached.
func OpenerFrom(ctx context.Context) Opener {
	o, _ := ctx.Value(openerKey{}).(Opener)
	return o
}

// Open applies the traffic guard and takes the snapshot.
func Open(ctx context.Context, globalDB *sql.DB, pdb *db.ProjectDB, projectDir string, projectID int) (*Snapshot, string, error) {
	p, err := models.GetProjectByID(globalDB, projectID)
	if err != nil {
		return nil, "", fmt.Errorf("flowsnap: project %d: %w", projectID, err)
	}
	domain := ""
	if p.Domain != nil {
		domain = *p.Domain
	}
	if !IsLocalDomain(domain) {
		busy, err := RecentRealTraffic(pdb.Reader(), trafficWindow)
		if err != nil {
			return nil, "", fmt.Errorf("flowsnap: checking recent traffic: %w", err)
		}
		if busy {
			return nil, fmt.Sprintf("real visitors used the app in the last %d minutes; a replay's data is rolled back afterwards, which would also roll back theirs, so the flows were not replayed", int(trafficWindow.Minutes())), nil
		}
	}
	snap, err := Take(ctx, pdb, projectDir)
	return snap, "", err
}

// Around runs replay between a snapshot and its restore, using the opener on ctx.
// A non-empty skip means the replay did not run: no opener in this context, the
// snapshot failed, or the traffic guard said no. The live data is never replayed
// against without a snapshot to restore. The restore runs even when ctx has been
// cancelled, and its error is returned separately: the replay's verdict still
// stands, but the caller must surface that the data was not fully rolled back.
func Around(ctx context.Context, replay func()) (skip string, restoreErr error) {
	open := OpenerFrom(ctx)
	if open == nil {
		return "no data snapshot is available here, so the flows were not replayed (their writes could not be rolled back)", nil
	}
	snap, skip, err := open(ctx)
	if err != nil {
		return "could not snapshot the app's data before the replay: " + err.Error(), nil
	}
	if skip != "" {
		return skip, nil
	}
	// Deferred so the data goes back even if the replay panics.
	defer func() { restoreErr = snap.Restore(context.WithoutCancel(ctx)) }()
	replay()
	return "", nil
}
