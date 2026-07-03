package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgyFile writes content to path under a host store, creating parents.
func writeAgyFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// buildAgyHostStore lays out a fake ~/.gemini/antigravity-cli with conversations
// for two workspaces and returns hostHome. Workspace A owns a1 (most recent) and
// a2 (history only); workspace B owns b1.
func buildAgyHostStore(t *testing.T, wsA, wsB string) string {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, ".gemini", "antigravity-cli")

	for _, id := range []string{"a1", "a2", "b1"} {
		writeAgyFile(t, filepath.Join(store, "conversations", id+".db"), "db-"+id)
		writeAgyFile(t, filepath.Join(store, "conversations", id+".db-wal"), "wal-"+id)
		writeAgyFile(t, filepath.Join(store, "brain", id, "note.txt"), "brain-"+id)
		writeAgyFile(t, filepath.Join(store, "implicit", id+".pb"), "imp-"+id)
	}

	writeAgyFile(t, filepath.Join(store, "cache", "last_conversations.json"),
		`{"`+wsA+`":"a1","`+wsB+`":"b1"}`)

	// history.jsonl: a2 and a1 attributed to A, b1 to B, plus a bare line with no
	// conversationId (must be tolerated and, for A, carried into the scoped copy).
	hist := `{"display":"x","workspace":"` + wsA + `","conversationId":"a2"}` + "\n" +
		`{"display":"y","workspace":"` + wsA + `"}` + "\n" +
		`{"display":"z","workspace":"` + wsA + `","conversationId":"a1"}` + "\n" +
		`{"display":"q","workspace":"` + wsB + `","conversationId":"b1"}` + "\n"
	writeAgyFile(t, filepath.Join(store, "history.jsonl"), hist)

	return home
}

func TestAgyWorkspaceConversations(t *testing.T) {
	wsA, wsB := "/home/joe/src/projA", "/home/joe/src/projB"
	home := buildAgyHostStore(t, wsA, wsB)
	hostStore := filepath.Join(home, ".gemini", "antigravity-cli")

	got := agyWorkspaceConversations(hostStore, wsA)
	if !got["a1"] || !got["a2"] {
		t.Errorf("expected a1 and a2 for workspace A, got %v", got)
	}
	if got["b1"] {
		t.Errorf("workspace B's conversation b1 leaked into workspace A set: %v", got)
	}
}

// TestPrepareAgyStoreScoping is the security-critical test: the scoped store for
// workspace A must contain A's conversations and none of B's.
func TestPrepareAgyStoreScoping(t *testing.T) {
	wsA, wsB := "/home/joe/src/projA", "/home/joe/src/projB"
	home := buildAgyHostStore(t, wsA, wsB)

	store, err := prepareAgyStore(home, wsA)
	if err != nil {
		t.Fatal(err)
	}

	mustExist := func(rel string) {
		if _, err := os.Stat(filepath.Join(store, rel)); err != nil {
			t.Errorf("expected %s in scoped store: %v", rel, err)
		}
	}
	mustAbsent := func(rel string) {
		if _, err := os.Stat(filepath.Join(store, rel)); err == nil {
			t.Errorf("%s must NOT be in scoped store (cross-project leak)", rel)
		}
	}

	// A's conversations and sidecars, brain, implicit are present.
	mustExist(filepath.Join("conversations", "a1.db"))
	mustExist(filepath.Join("conversations", "a1.db-wal"))
	mustExist(filepath.Join("conversations", "a2.db"))
	mustExist(filepath.Join("brain", "a1", "note.txt"))
	mustExist(filepath.Join("implicit", "a1.pb"))

	// B's conversation must be invisible in every form.
	mustAbsent(filepath.Join("conversations", "b1.db"))
	mustAbsent(filepath.Join("conversations", "b1.db-wal"))
	mustAbsent(filepath.Join("brain", "b1"))
	mustAbsent(filepath.Join("implicit", "b1.pb"))

	// Scoped recency map holds only A's entry.
	recent, err := readAgyRecency(store)
	if err != nil {
		t.Fatal(err)
	}
	if recent[wsA] != "a1" {
		t.Errorf("scoped recency should map %s->a1, got %v", wsA, recent)
	}
	if _, ok := recent[wsB]; ok {
		t.Errorf("scoped recency leaked workspace B: %v", recent)
	}

	// Scoped history holds only A's lines.
	histData, err := os.ReadFile(filepath.Join(store, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	hist := string(histData)
	if strings.Contains(hist, wsB) {
		t.Errorf("scoped history leaked workspace B lines:\n%s", hist)
	}
	if !strings.Contains(hist, `"a1"`) || !strings.Contains(hist, `"a2"`) {
		t.Errorf("scoped history missing A's conversations:\n%s", hist)
	}
}

// TestPrepareAgyStoreSeedsOnce verifies re-invocation does not clobber a
// conversation the user continued inside flar after the initial seed.
func TestPrepareAgyStoreSeedsOnce(t *testing.T) {
	wsA := "/home/joe/src/projA"
	home := buildAgyHostStore(t, wsA, "/home/joe/src/projB")

	store, err := prepareAgyStore(home, wsA)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an in-flar edit to a1 after seeding.
	edited := filepath.Join(store, "conversations", "a1.db")
	if err := os.WriteFile(edited, []byte("edited-in-flar"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareAgyStore(home, wsA); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "edited-in-flar" {
		t.Errorf("second prepareAgyStore re-seeded and clobbered a1.db; got %q", got)
	}
}

func TestCopyDirExceptSkips(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")
	writeAgyFile(t, filepath.Join(src, "keep.txt"), "keep")
	writeAgyFile(t, filepath.Join(src, "antigravity-cli", "settings.json"), "cfg")
	writeAgyFile(t, filepath.Join(src, "antigravity-cli", "conversations", "x.db"), "secret")
	writeAgyFile(t, filepath.Join(src, "antigravity-cli", ".flar", "slug", "conversations", "y.db"), "secret2")

	if err := CopyDirExcept(src, dst, agySkipCopy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Errorf("keep.txt should be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "antigravity-cli", "settings.json")); err != nil {
		t.Errorf("settings.json should be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "antigravity-cli", "conversations")); err == nil {
		t.Errorf("conversations/ must be skipped")
	}
	if _, err := os.Stat(filepath.Join(dst, "antigravity-cli", ".flar")); err == nil {
		t.Errorf(".flar/ (all workspaces' scoped stores) must be skipped")
	}
}
