package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// agyStoreDirs are the antigravity-cli subdirectories holding per-conversation
// state. flar scopes each to a single workspace so a sandboxed agy can never read
// or resume another project's conversations.
//   - conversations: the conversation records (SQLite .db + sidecars, legacy .pb)
//   - brain:         per-conversation memory keyed by conversation UUID
//   - implicit:      implicitly captured conversation context (.pb)
var agyStoreDirs = []string{"conversations", "brain", "implicit"}

// agyStoreRel is the store root, relative to ~/.gemini/antigravity-cli, under which
// flar keeps one private conversation store per workspace slug. It lives inside
// antigravity-cli for locality but is excluded from the copied sandbox config (see
// agySkipCopy) so no workspace's store leaks into another's sandbox.
const agyStoreRel = ".flar"

// prepareAgyStore returns the path to this workspace's private agy conversation
// store, creating it and — once — seeding it from the host's global store. The
// store is bind-mounted over ~/.gemini/antigravity-cli inside the sandbox so
// conversations created in flar persist and can be resumed, while conversations
// from other projects stay invisible.
//
// Seeding copies only conversations agy itself attributes to this workspace, read
// from two plain-text host indices (cache/last_conversations.json and
// history.jsonl) rather than by parsing agy's opaque conversation blobs. After the
// one-time seed the store diverges: new flar sessions accumulate here and later
// host-side changes are not pulled back in.
func prepareAgyStore(hostHome, absProjectDir string) (string, error) {
	hostStore := filepath.Join(hostHome, ".gemini", "antigravity-cli")
	store := filepath.Join(hostStore, agyStoreRel, claudeProjectSlug(absProjectDir))

	for _, sub := range agyStoreDirs {
		if err := os.MkdirAll(filepath.Join(store, sub), 0o700); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Join(store, "cache"), 0o700); err != nil {
		return "", err
	}

	// Seed exactly once; the marker guards against re-seeding (which would clobber
	// conversations the user has since continued inside flar).
	marker := filepath.Join(store, ".seeded")
	if _, err := os.Stat(marker); err != nil {
		seedAgyStore(hostStore, store, absProjectDir) // best-effort
		if f, err := os.OpenFile(marker, os.O_CREATE, 0o600); err == nil {
			f.Close()
		}
	}

	// The index files must exist so they can be bind-mounted even when the seed
	// found nothing (a fresh project).
	ensureFile(filepath.Join(store, "history.jsonl"))
	ensureFile(filepath.Join(store, "cache", "last_conversations.json"))
	return store, nil
}

// seedAgyStore copies this workspace's existing host conversations into its scoped
// store, along with scoped copies of the recency and history indices.
func seedAgyStore(hostStore, store, absProjectDir string) {
	ids := agyWorkspaceConversations(hostStore, absProjectDir)
	for id := range ids {
		// Conversation record plus its SQLite sidecars and any legacy protobuf.
		for _, ext := range []string{".db", ".db-wal", ".db-shm", ".pb"} {
			src := filepath.Join(hostStore, "conversations", id+ext)
			if fileExists(src) {
				_ = CopyFile(src, filepath.Join(store, "conversations", id+ext))
			}
		}
		if src := filepath.Join(hostStore, "brain", id); dirExists(src) {
			_ = CopyDir(src, filepath.Join(store, "brain", id))
		}
		if src := filepath.Join(hostStore, "implicit", id+".pb"); fileExists(src) {
			_ = CopyFile(src, filepath.Join(store, "implicit", id+".pb"))
		}
	}
	seedAgyRecency(hostStore, store, absProjectDir)
	seedAgyHistory(hostStore, store, absProjectDir)
}

// agyWorkspaceConversations returns the set of conversation UUIDs the host store
// attributes to absProjectDir, unioned from the recency map and the prompt
// history. Both are plain text written by agy, so this needs no knowledge of the
// conversation blob format. Conversations not attributed to this workspace are
// omitted, which is the safe default for a security boundary.
func agyWorkspaceConversations(hostStore, absProjectDir string) map[string]bool {
	out := map[string]bool{}

	if recent, err := readAgyRecency(hostStore); err == nil {
		if id := recent[absProjectDir]; id != "" {
			out[id] = true
		}
	}

	f, err := os.Open(filepath.Join(hostStore, "history.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e struct {
			Workspace      string `json:"workspace"`
			ConversationID string `json:"conversationId"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Workspace == absProjectDir && e.ConversationID != "" {
			out[e.ConversationID] = true
		}
	}
	return out
}

// seedAgyRecency writes a scoped last_conversations.json holding only this
// workspace's most-recent entry, so `agy --continue` targets the right
// conversation and cannot see other workspaces' recency entries.
func seedAgyRecency(hostStore, store, absProjectDir string) {
	recent, err := readAgyRecency(hostStore)
	if err != nil {
		return
	}
	id, ok := recent[absProjectDir]
	if !ok || id == "" {
		return
	}
	if out, err := json.Marshal(map[string]string{absProjectDir: id}); err == nil {
		_ = os.WriteFile(filepath.Join(store, "cache", "last_conversations.json"), out, 0o600)
	}
}

// seedAgyHistory writes a scoped history.jsonl containing only this workspace's
// prompt-history lines, keeping other projects' prompts out of the picker.
func seedAgyHistory(hostStore, store, absProjectDir string) {
	f, err := os.Open(filepath.Join(hostStore, "history.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	var b bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e struct {
			Workspace string `json:"workspace"`
		}
		line := sc.Bytes()
		if json.Unmarshal(line, &e) == nil && e.Workspace == absProjectDir {
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	if b.Len() > 0 {
		_ = os.WriteFile(filepath.Join(store, "history.jsonl"), b.Bytes(), 0o600)
	}
}

// readAgyRecency parses cache/last_conversations.json (workspace path -> most
// recent conversation UUID).
func readAgyRecency(hostStore string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(hostStore, "cache", "last_conversations.json"))
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
