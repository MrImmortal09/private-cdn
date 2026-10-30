package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextCreateEditAndShare(t *testing.T) {
	_, h, root := newTestServer(t)
	c := login(t, h)
	js := map[string]string{"Origin": testURL, "Content-Type": "application/json"}
	call := func(target string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		return do(h, "POST", target, bytes.NewReader(b), js, c)
	}
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}

	if rec := do(h, "GET", "/new?dir=", nil, nil, c); rec.Code != 200 || !strings.Contains(rec.Body.String(), "New text file") {
		t.Fatalf("new page: %d", rec.Code)
	}
	if rec := do(h, "GET", "/new?dir=nope", nil, nil, c); rec.Code != 404 {
		t.Fatalf("new page for a missing folder: %d", rec.Code)
	}

	// A name without an extension gets .txt; an existing name is never overwritten.
	rec := call("/api/text/create", map[string]string{"Dir": "", "Name": "notes", "Text": "hello\n"})
	if rec.Code != 200 || decode(rec)["path"] != "notes.txt" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	share := decode(rec)["url"].(string)
	if rec := call("/api/text/create", map[string]string{"Name": "notes.txt", "Text": "other"}); decode(rec)["path"] != "notes (2).txt" {
		t.Fatalf("second create: %s", rec.Body)
	}
	if rec := call("/api/text/create", map[string]string{"Text": "x"}); !strings.HasPrefix(decode(rec)["path"].(string), "paste-") {
		t.Fatalf("unnamed create: %s", rec.Body)
	}
	if rec := call("/api/text/create", map[string]string{"Name": ".hidden", "Text": "x"}); rec.Code != 400 {
		t.Fatalf("hidden name: %d", rec.Code)
	}

	// The share link serves the text without a login.
	sharePath := strings.TrimPrefix(share, testURL)
	if rec := do(h, "GET", sharePath, nil, nil, nil); rec.Body.String() != "hello\n" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("share: %q %s", rec.Body, rec.Header().Get("Content-Type"))
	}

	// The editor shows the text and the share link; the listing links to it.
	rec = do(h, "GET", "/edit/notes.txt", nil, nil, c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), ">\nhello\n</textarea>") || !strings.Contains(rec.Body.String(), share) {
		t.Fatalf("edit page: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/edit/notes.txt", nil, nil, nil); rec.Code != 302 {
		t.Fatalf("edit page without login: %d", rec.Code)
	}
	if rec := do(h, "GET", "/files/", nil, nil, c); !strings.Contains(rec.Body.String(), `href="/edit/notes.txt"`) {
		t.Fatal("listing has no Edit link for notes.txt")
	}

	// Saving needs the version the editor loaded, unless forced.
	v := textVersion([]byte("hello\n"))
	if rec := call("/api/text/save", map[string]any{"Path": "notes.txt", "Text": "stale", "Version": "0000"}); rec.Code != 409 {
		t.Fatalf("stale save: %d", rec.Code)
	}
	rec = call("/api/text/save", map[string]any{"Path": "notes.txt", "Text": "hello again\n", "Version": v})
	if rec.Code != 200 || decode(rec)["version"] != textVersion([]byte("hello again\n")) {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if rec := call("/api/text/save", map[string]any{"Path": "notes.txt", "Text": "forced", "Force": true}); rec.Code != 200 {
		t.Fatalf("forced save: %d", rec.Code)
	}
	if rec := do(h, "GET", sharePath, nil, nil, nil); rec.Body.String() != "forced" {
		t.Fatalf("share link after edit: %q", rec.Body)
	}
	if des, _ := os.ReadDir(root); len(des) != 3 {
		t.Fatalf("expected only the 3 created files, got %d entries (temp file left behind?)", len(des))
	}

	// Cross-site saves are refused.
	b, _ := json.Marshal(map[string]any{"Path": "notes.txt", "Text": "evil", "Force": true})
	if rec := do(h, "POST", "/api/text/save", bytes.NewReader(b), map[string]string{"Origin": "https://evil.example"}, c); rec.Code != 403 {
		t.Fatalf("cross-site save: %d", rec.Code)
	}
}

func TestTextEditorEdgeCases(t *testing.T) {
	_, h, root := newTestServer(t)
	c := login(t, h)
	js := map[string]string{"Origin": testURL, "Content-Type": "application/json"}
	save := func(body any) int {
		b, _ := json.Marshal(body)
		return do(h, "POST", "/api/text/save", bytes.NewReader(b), js, c).Code
	}

	// Markup in the text stays text, and a leading newline survives the textarea.
	os.WriteFile(filepath.Join(root, "x.html"), []byte("\n</textarea><script>alert(1)</script>"), 0o644)
	rec := do(h, "GET", "/edit/x.html", nil, nil, c)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "<script>alert") || !strings.Contains(rec.Body.String(), ">\n\n&lt;/textarea&gt;") {
		t.Fatalf("edit page escaping: %d %s", rec.Code, rec.Body)
	}

	// Windows line endings are kept.
	os.WriteFile(filepath.Join(root, "win.txt"), []byte("a\r\nb\r\n"), 0o600)
	if code := save(map[string]any{"Path": "win.txt", "Text": "a\nb\nc\n", "Version": textVersion([]byte("a\r\nb\r\n"))}); code != 200 {
		t.Fatalf("save win.txt: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "win.txt")); string(b) != "a\r\nb\r\nc\r\n" {
		t.Fatalf("CRLF not kept: %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(root, "win.txt")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}

	// Binary files, zip entries, folders, hidden and outside paths are not editable.
	os.WriteFile(filepath.Join(root, "bin.txt"), []byte{0, 1, 2}, 0o644)
	writeZip(t, filepath.Join(root, "z.zip"), map[string][]byte{"in.txt": []byte("hi")}, 0)
	os.Mkdir(filepath.Join(root, "dir"), 0o755)
	for _, p := range []string{"bin.txt", "z.zip/in.txt", "dir", "missing.txt"} {
		if rec := do(h, "GET", "/edit/"+p, nil, nil, c); rec.Code == 200 {
			t.Fatalf("edit page for %s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"z.zip/in.txt", "dir", "", "../etc/passwd", ".hidden"} {
		if code := save(map[string]any{"Path": p, "Text": "x", "Force": true}); code == 200 {
			t.Fatalf("save to %q allowed", p)
		}
	}
	if code := save(map[string]any{"Path": "win.txt", "Text": strings.Repeat("x", maxTextSize+1), "Force": true}); code != 413 {
		t.Fatalf("oversized save: %d", code)
	}

	// The listing offers Edit only for text files outside zips.
	os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("v"), 0o644)
	body := do(h, "GET", "/files/", nil, nil, c).Body.String()
	if strings.Contains(body, `href="/edit/clip.mp4"`) || !strings.Contains(body, `href="/edit/win.txt"`) {
		t.Fatal("wrong Edit links in listing")
	}
	if body := do(h, "GET", "/files/z.zip/", nil, nil, c).Body.String(); strings.Contains(body, "/edit/") {
		t.Fatal("Edit link inside a zip")
	}
}
