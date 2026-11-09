package engine

// Pins the planner-declared leaf-interface (`exports`) round-trip: a task's
// exports must survive seed -> of_tasks -> LoadTasks and surface in the build
// checklist, so a consumer leaf reads the exact accessor to import instead of
// stubbing it on a window global (the window.__peerDist dead-global defect).

import (
	"strings"
	"testing"

	"openfabrica/db"
)

func TestTaskExportsRoundTripAndRender(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	seed := []Task{
		{
			ID:      "remote-avatars",
			Title:   "Remote players & roster",
			Intent:  "Track peer positions and roster.",
			Paths:   []string{"/files/world.remote.js"},
			Exports: []string{"initRemote(engine)", "peerDistance(id, localPos) -> number"},
		},
		{
			ID:     "chat-emotes",
			Title:  "Proximity chat",
			Intent: "Dim distant senders.",
			Deps:   []string{"remote-avatars"},
			Paths:  []string{"/files/world.chat.js"},
		},
	}
	if _, err := AddTasks(pdb, "run_test", seed); err != nil {
		t.Fatalf("AddTasks: %v", err)
	}

	// Round-trip: exports survive the JSON-encoded column.
	loaded, err := LoadTasks(pdb.Reader())
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	var remote *Task
	for i := range loaded {
		if loaded[i].ID == "remote-avatars" {
			remote = &loaded[i]
		}
	}
	if remote == nil {
		t.Fatal("remote-avatars task did not load")
	}
	if got := strings.Join(remote.Exports, "|"); !strings.Contains(got, "peerDistance(id, localPos) -> number") {
		t.Fatalf("exports did not round-trip; got %q", got)
	}

	// Render: the checklist shows the producer's interface so the consumer imports it.
	cl := renderChecklist(pdb.Reader())
	if !strings.Contains(cl, "Exposes") || !strings.Contains(cl, "peerDistance(id, localPos) -> number") {
		t.Fatalf("checklist must surface the leaf interface under Exposes; got:\n%s", cl)
	}

	// The checklist pre-assembles the exact import a consumer pastes - bare symbol
	// names from the exposed signatures, under the module's bare specifier - so it never
	// has to construct the line and reach for a window.* stub.
	if !strings.Contains(cl, "import { initRemote, peerDistance } from 'world.remote';") {
		t.Fatalf("checklist must pre-assemble the interface import line; got:\n%s", cl)
	}

	// A task with no exports (the consumer) must not emit an empty Exposes line.
	chatLine := ""
	for _, line := range strings.Split(cl, "\n- ") {
		if strings.Contains(line, "chat-emotes") {
			chatLine = line
		}
	}
	if chatLine == "" {
		t.Fatal("chat-emotes not rendered")
	}
	if strings.Contains(chatLine, "Exposes") {
		t.Errorf("a task with no exports must not render an Exposes line; got:\n%s", chatLine)
	}
}

func TestImportScaffoldLine(t *testing.T) {
	got := importScaffoldLine(
		[]string{"initRemote(engine)", "peerDistance(id, localPos) -> number"},
		[]string{"/files/world.remote.js"},
	)
	want := "import { initRemote, peerDistance } from 'world.remote';"
	if got != want {
		t.Fatalf("importScaffoldLine = %q, want %q", got, want)
	}
	// A packed entry (several symbols joined into one string) still yields every symbol.
	got = importScaffoldLine([]string{"open(id)/close(id)"}, []string{"/files/panel.js"})
	if want := "import { open, close } from 'panel';"; got != want {
		t.Fatalf("importScaffoldLine packed = %q, want %q", got, want)
	}
	// A task owning no JS module (styles only) yields no scaffold.
	if got := importScaffoldLine([]string{"tokens"}, []string{"/files/app.css"}); got != "" {
		t.Errorf("a task owning no JS module must yield no scaffold, got %q", got)
	}
	// No extractable symbol yields no scaffold.
	if got := importScaffoldLine([]string{"-> number"}, []string{"/files/x.js"}); got != "" {
		t.Errorf("no extractable symbol must yield no scaffold, got %q", got)
	}
}

func TestExportSymbolNames(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"peerDistance(id, localPos) -> number", "peerDistance"},
		{"addPeer(p)/removePeer(id)/getPeer(id)", "addPeer,removePeer,getPeer"},
		{"storedName()/storeName(name)", "storedName,storeName"},
		{"buildWorld(scene) -> adds terrain/zones/props; groundHeightAt(x,z) -> number", "buildWorld,groundHeightAt"},
		{"initChat(net) -> wires input/log/toasts; systemMsg(text) log+toast; sendEmote(n)", "initChat,systemMsg,sendEmote"},
		{"CONSTANTS {a, b}", "CONSTANTS"},
		{"does(x); also seeds rows", "does"},
		{"", ""},
	}
	for _, c := range cases {
		if got := strings.Join(exportSymbolNames(c.in), ","); got != c.want {
			t.Errorf("exportSymbolNames(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExportSymbolName(t *testing.T) {
	cases := map[string]string{
		"peerDistance(id, localPos) -> number": "peerDistance",
		"initRemote(engine)":                   "initRemote",
		"SCENE":                                "SCENE",
		"$mount()":                             "$mount",
		"3d":                                   "",
		"":                                     "",
	}
	for in, want := range cases {
		if got := exportSymbolName(in); got != want {
			t.Errorf("exportSymbolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The scaffold line never names the page entry: a sibling that pastes it would import
// from the unimported root, which the save gate rejects (a build's checklist said
// `import { mountRegion, store } from 'badsanta';`).
func TestExportHomePaths_NeverTheEntry(t *testing.T) {
	cases := []struct {
		name   string
		paths  []string
		edited map[string]bool
		want   string
	}{
		{"deps-wired entry, no store", []string{"/", "/files/badsanta.js", "/files/badsanta.css"}, nil, ""},
		{"edited entry plus store", []string{"/", "/files/game.js", "/files/game.state.js"}, map[string]bool{"game.js": true},
			"import { a } from 'game.state';"},
		{"store edited by leaves", []string{"/", "/files/board.state.js"}, map[string]bool{"board.state.js": true},
			"import { a } from 'board.state';"},
		{"leaf task", []string{"/files/world.remote.js"}, nil, "import { a } from 'world.remote';"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := importScaffoldLine([]string{"a()"}, exportHomePaths(c.paths, c.edited)); got != c.want {
				t.Errorf("scaffold = %q, want %q", got, c.want)
			}
		})
	}
}
