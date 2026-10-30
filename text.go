package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Text files can be created and edited in the browser, like a pastebin. A
// share link is derived from the path, so it keeps working across edits and
// always serves the latest text.

const maxTextSize = 1 << 20

var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".log": true, ".csv": true, ".tsv": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".conf": true, ".cfg": true,
	".env": true, ".xml": true, ".html": true, ".htm": true, ".css": true, ".js": true, ".ts": true,
	".sh": true, ".py": true, ".go": true, ".rs": true, ".c": true, ".h": true, ".cpp": true,
	".java": true, ".sql": true, ".srt": true, ".vtt": true,
}

// isText says whether the listing offers to edit a file. The editor itself
// accepts any file whose contents are text.
func isText(name string) bool { return textExts[strings.ToLower(path.Ext(name))] }

func looksLikeText(b []byte) bool { return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 }

// textVersion identifies a file's contents, so a save can tell whether the
// file changed after the editor loaded it.
func textVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type crumb struct{ Name, Href string }

func crumbsFor(rel string) []crumb {
	crumbs := []crumb{{"files", "/files/"}}
	acc := ""
	if rel != "" {
		for _, p := range strings.Split(rel, "/") {
			acc += p + "/"
			crumbs = append(crumbs, crumb{p, "/files/" + escapePath(strings.TrimSuffix(acc, "/")) + "/"})
		}
	}
	return crumbs
}

func parentRel(rel string) string {
	if d := path.Dir(rel); d != "." {
		return d
	}
	return ""
}

// handleNewText shows an empty editor for a new file in ?dir=.
func (s *server) handleNewText(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanRel(r.URL.Query().Get("dir"))
	if err == nil {
		_, err = s.realDir(rel)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "edit", map[string]any{"New": true, "Dir": rel, "Crumbs": crumbsFor(rel)})
}

// editable resolves rel to a regular file outside any zip and returns its real
// path, so editing a symlink edits the file it points to.
func (s *server) editable(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	t, err := s.resolve(rel)
	if err != nil || t.isDir || t.inZip {
		return "", false
	}
	real, err := filepath.EvalSymlinks(t.fsPath)
	if err != nil {
		return "", false
	}
	return real, true
}

func (s *server) handleEditText(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanRel(r.PathValue("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	real, ok := s.editable(rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	fi, err := os.Stat(real)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fi.Size() > maxTextSize {
		http.Error(w, "This file is too big to edit here (over 1 MB).", http.StatusUnprocessableEntity)
		return
	}
	b, err := os.ReadFile(real)
	if err != nil {
		http.Error(w, "cannot read file", http.StatusInternalServerError)
		return
	}
	if !looksLikeText(b) {
		http.Error(w, "This is not a text file.", http.StatusUnprocessableEntity)
		return
	}
	s.render(w, "edit", map[string]any{
		"Path": rel, "Name": path.Base(rel), "Crumbs": crumbsFor(parentRel(rel)),
		"Text": string(b), "Version": textVersion(b),
		"Raw": "/files/" + escapePath(rel), "Share": s.shares.url(s.cfg.PublicURL, rel),
	})
}

func (s *server) handleCreateText(w http.ResponseWriter, r *http.Request) {
	var req struct{ Dir, Name, Text string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*maxTextSize)).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if len(req.Text) > maxTextSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "text is over 1 MB"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "paste-" + time.Now().Format("2006-01-02-150405")
	}
	name, err := cleanName(req.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid file name"})
		return
	}
	// Without an extension the file would be served as a download, not text.
	if path.Ext(name) == "" {
		name += ".txt"
	}
	rel, err := cleanRel(req.Dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad folder"})
		return
	}
	dir, err := s.realDir(rel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such folder"})
		return
	}
	dest, err := writeAtomically(dir, name, strings.NewReader(req.Text))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rel = s.relOf(dest)
	log.Printf("new text: %s", rel)
	writeJSON(w, http.StatusOK, map[string]string{
		"path": rel, "edit": "/edit/" + escapePath(rel), "url": s.shares.url(s.cfg.PublicURL, rel),
	})
}

func (s *server) handleSaveText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path, Text, Version string
		Force               bool // overwrite even if the file changed since it was loaded
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*maxTextSize)).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if len(req.Text) > maxTextSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "text is over 1 MB"})
		return
	}
	rel, err := cleanRel(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad path"})
		return
	}
	real, ok := s.editable(rel)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this file no longer exists"})
		return
	}

	s.textMu.Lock()
	defer s.textMu.Unlock()
	fi, err := os.Stat(real)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this file no longer exists"})
		return
	}
	cur, err := os.ReadFile(real)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !req.Force && textVersion(cur) != req.Version {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this file changed after you opened it"})
		return
	}
	text := []byte(req.Text)
	// Browsers turn every line ending into \n; keep a Windows file's \r\n.
	if bytes.Contains(cur, []byte("\r\n")) && !bytes.ContainsRune(text, '\r') {
		text = bytes.ReplaceAll(text, []byte("\n"), []byte("\r\n"))
	}

	// Write a hidden temp file and rename it over the original, so readers
	// never see a half-written file.
	tmp, err := os.CreateTemp(filepath.Dir(real), "."+filepath.Base(real)+".*.part")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_, err = tmp.Write(text)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), fi.Mode().Perm())
	}
	if err == nil {
		err = os.Rename(tmp.Name(), real)
	}
	if err != nil {
		os.Remove(tmp.Name())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("edit text: %s", rel)
	writeJSON(w, http.StatusOK, map[string]any{"version": textVersion(text), "size": len(text)})
}
