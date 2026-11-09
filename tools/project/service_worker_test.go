package project

import (
	"os"
	"strings"
	"testing"

	"openfabrica/tools/tooltest"
)

// The platform-generated service worker is the only worker a project gets, so it
// must show the push manage_push sends and open the target on click, or a
// subscription is stored and every notification is silently dropped.
func TestEnablePWAServiceWorkerHandlesPush(t *testing.T) {
	h := tooltest.New(t)
	if _, err := h.Global.DB.Exec(`INSERT INTO projects (id, name, published, mode, llm_model_id, ambition_level) VALUES (?, 'p', 1, 'building', 1, 'standard')`, tooltest.ProjectID); err != nil {
		t.Fatal(err)
	}
	seedFile(t, h.DB(), "page", "index")

	h.MustOK("manage_project", map[string]any{"action": "enable_pwa"})

	var diskPath string
	if err := h.DB().QueryRow(`SELECT disk_path FROM of_files WHERE kind = 'file' AND path = 'service-worker.js'`).Scan(&diskPath); err != nil {
		t.Fatalf("service worker not saved: %v", err)
	}
	src, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"addEventListener('push'", "showNotification", "addEventListener('notificationclick'", "openWindow"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("service worker missing %q", want)
		}
	}
}
