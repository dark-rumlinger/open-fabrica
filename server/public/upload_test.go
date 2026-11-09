package public

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
)

// TestWriteExclusive_FailsOnExisting pins the load-bearing behaviour of the
// O_EXCL switch: if a file with the same name already sits on disk, the
// second writer must fail with os.ErrExist rather than silently overwriting.
// Without this, two same-nanosecond concurrent uploads would clobber each
// other and the first uploader's URL would serve the second uploader's
// bytes - a cross-user data leak.
func TestWriteExclusive_FailsOnExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := writeExclusive(path, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	err := writeExclusive(path, []byte("second"))
	if err == nil {
		t.Fatalf("second write succeeded; want os.ErrExist")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("second write err = %v; want os.ErrExist", err)
	}
	// Bytes on disk must be the first writer's, not the second's.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "first" {
		t.Errorf("file contents = %q; want %q (second writer overwrote first)", string(got), "first")
	}
}

// TestWriteExclusive_ConcurrentCollision runs many writers against the same
// path in parallel and confirms exactly one wins, the rest see ErrExist - the
// race an exclusive create has to settle.
func TestWriteExclusive_ConcurrentCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "race")

	const writers = 32
	var (
		successes int
		exists    int
		other     int
		mu        sync.Mutex
		wg        sync.WaitGroup
		start     = make(chan struct{})
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := writeExclusive(path, []byte{byte(i)})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, os.ErrExist):
				exists++
			default:
				other++
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Errorf("successes = %d; want exactly 1", successes)
	}
	if exists != writers-1 {
		t.Errorf("ErrExist count = %d; want %d", exists, writers-1)
	}
	if other != 0 {
		t.Errorf("unexpected non-ErrExist errors: %d", other)
	}
}

// TestRandomHex4_Shape locks the suffix format (8 hex chars) so a future
// refactor of randomHex4 can't silently shorten the entropy.
func TestRandomHex4_Shape(t *testing.T) {
	pat := regexp.MustCompile(`^[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		s := randomHex4()
		if !pat.MatchString(s) {
			t.Fatalf("randomHex4() = %q; want 8 hex chars", s)
		}
		seen[s] = true
	}
	// 1000 draws from a 4-byte (~4 billion) space should produce ~1000
	// distinct values - allow a tiny floor so this isn't flaky.
	if len(seen) < 990 {
		t.Errorf("randomHex4 produced %d distinct values out of 1000; entropy looks broken", len(seen))
	}
}

// An upload linked to a table returns the row it inserted, with the columns the table
// generates. A share-link app needs the slug the table mints; without the row the page
// had no way to learn it and the builder rebuilt the upload around a second endpoint.
func TestUploadLinkedTableReturnsInsertedRow(t *testing.T) {
	h, pdb := newUploadRateLimitHandler(t, 0)
	for _, s := range []string{
		`CREATE TABLE files (id INTEGER PRIMARY KEY, filename TEXT, url TEXT, content_type TEXT, size INTEGER,
			slug TEXT NOT NULL DEFAULT (lower(hex(randomblob(4)))))`,
		`UPDATE of_upload_endpoints SET table_name = 'files' WHERE path = 'photos'`,
	} {
		if _, err := pdb.Writer().Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	w := httptest.NewRecorder()
	h.apiUpload(w, uploadRequest(t, "192.0.2.10"), 1, pdb, "photos")
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		URL string         `json:"url"`
		Row map[string]any `json:"row"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var slug string
	if err := pdb.Reader().QueryRow(`SELECT slug FROM files`).Scan(&slug); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if resp.Row["slug"] != slug || resp.Row["url"] != resp.URL || resp.Row["id"] == nil {
		t.Errorf("row = %v, want the stored row with slug %q and url %q", resp.Row, slug, resp.URL)
	}
}
