package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var errNotFound = errors.New("not found")

// cleanRel turns a URL path into a clean path relative to the root. It rejects
// "..", and hidden names (partial downloads and uploads are hidden files).
func cleanRel(p string) (string, error) {
	var out []string
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "" || part == ".":
		case part == ".." || strings.HasPrefix(part, ".") || strings.ContainsRune(part, 0):
			return "", errNotFound
		default:
			out = append(out, part)
		}
	}
	return strings.Join(out, "/"), nil
}

func isZip(name string) bool { return strings.EqualFold(path.Ext(name), ".zip") }

// target is what a relative path points at: a directory, a file, or (when the
// path goes through a .zip) a zip plus a path inside it.
type target struct {
	rel    string
	fsPath string // the directory, the file, or the zip itself
	isDir  bool
	size   int64
	mod    time.Time
	inZip  bool
	inner  string // path inside the zip; "" is the zip's top level
}

func (s *server) within(p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	return real == s.root || strings.HasPrefix(real, s.root+string(filepath.Separator))
}

func (s *server) resolve(rel string) (*target, error) {
	parts := []string{}
	if rel != "" {
		parts = strings.Split(rel, "/")
	}
	cur := s.root
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Stat(cur)
		if err != nil {
			return nil, errNotFound
		}
		if fi.IsDir() {
			continue
		}
		if !fi.Mode().IsRegular() || !s.within(cur) {
			return nil, errNotFound
		}
		t := &target{rel: rel, fsPath: cur, size: fi.Size(), mod: fi.ModTime()}
		if i == len(parts)-1 {
			return t, nil
		}
		if !isZip(part) {
			return nil, errNotFound
		}
		t.inZip, t.inner = true, strings.Join(parts[i+1:], "/")
		return t, nil
	}
	if !s.within(cur) {
		return nil, errNotFound
	}
	return &target{rel: rel, fsPath: cur, isDir: true}, nil
}

// ---------- zip archives ----------

type zipArchive struct {
	f     *os.File
	r     *zip.Reader
	mod   time.Time
	size  int64
	files map[string]*zip.File // cleaned name -> entry (files only)
	dirs  map[string]bool      // every folder implied by the entries

	refs    int  // open() calls not yet release()d; guarded by zipCache.mu
	dropped bool // no longer in the cache: close once refs reaches 0
}

type zipCache struct {
	mu  sync.Mutex
	max int
	m   map[string]*zipArchive
}

func newZipCache(max int) *zipCache { return &zipCache{max: max, m: map[string]*zipArchive{}} }

// open returns the parsed zip, reusing it while the file is unchanged. Every
// open must be paired with release once its reads are done; an archive dropped
// from the cache is closed when its last reader releases it.
func (c *zipCache) open(p string) (*zipArchive, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.m[p]; a != nil && a.mod.Equal(fi.ModTime()) && a.size == fi.Size() {
		a.refs++
		return a, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	a := &zipArchive{f: f, r: zr, mod: fi.ModTime(), size: fi.Size(),
		files: map[string]*zip.File{}, dirs: map[string]bool{"": true}}
	for _, zf := range zr.File {
		name := strings.Trim(strings.ReplaceAll(zf.Name, "\\", "/"), "/")
		name = strings.TrimPrefix(name, "./")
		if name == "" {
			continue
		}
		if strings.HasSuffix(zf.Name, "/") || zf.FileInfo().IsDir() {
			a.dirs[name] = true
		} else {
			a.files[name] = zf
		}
		for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
			a.dirs[d] = true
		}
	}
	if old := c.m[p]; old != nil {
		c.drop(p, old)
	} else if len(c.m) >= c.max {
		for k, old := range c.m {
			c.drop(k, old)
			break
		}
	}
	c.m[p] = a
	a.refs++
	return a, nil
}

// drop removes a from the cache, closing it unless a reader still holds it.
// c.mu must be held.
func (c *zipCache) drop(p string, a *zipArchive) {
	delete(c.m, p)
	a.dropped = true
	if a.refs == 0 {
		a.f.Close()
	}
}

func (c *zipCache) release(a *zipArchive) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a.refs--
	if a.refs == 0 && a.dropped {
		a.f.Close()
	}
}

type entry struct {
	Name      string
	Href      string
	IsDir     bool
	IsZip     bool
	Size      int64
	Share     string
	Deletable bool
}

func (a *zipArchive) children(inner string) []entry {
	prefix := ""
	if inner != "" {
		prefix = inner + "/"
	}
	seen := map[string]bool{}
	var out []entry
	add := func(name string, isDir bool, size int64) {
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, entry{Name: name, IsDir: isDir, Size: size})
	}
	for d := range a.dirs {
		if d != "" && strings.HasPrefix(d, prefix) && !strings.Contains(d[len(prefix):], "/") {
			add(d[len(prefix):], true, 0)
		}
	}
	for n, zf := range a.files {
		if strings.HasPrefix(n, prefix) && !strings.Contains(n[len(prefix):], "/") {
			add(n[len(prefix):], false, int64(zf.UncompressedSize64))
		}
	}
	return out
}

// inflateSeeker gives http.ServeContent a ReadSeeker over a compressed zip
// entry. Seeking forward skips decompressed bytes; seeking back reopens.
type inflateSeeker struct {
	zf        *zip.File
	size      int64
	pos, rpos int64
	rc        io.ReadCloser
}

func (s *inflateSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += s.pos
	case io.SeekEnd:
		off += s.size
	}
	if off < 0 {
		return 0, errors.New("negative seek")
	}
	s.pos = off
	return off, nil
}

func (s *inflateSeeker) Read(p []byte) (int, error) {
	if s.pos >= s.size {
		return 0, io.EOF
	}
	if s.rc == nil || s.pos < s.rpos {
		s.Close()
		rc, err := s.zf.Open()
		if err != nil {
			return 0, err
		}
		s.rc, s.rpos = rc, 0
	}
	if s.pos > s.rpos {
		if _, err := io.CopyN(io.Discard, s.rc, s.pos-s.rpos); err != nil {
			return 0, err
		}
		s.rpos = s.pos
	}
	n, err := s.rc.Read(p)
	s.pos += int64(n)
	s.rpos += int64(n)
	return n, err
}

func (s *inflateSeeker) Close() error {
	if s.rc != nil {
		err := s.rc.Close()
		s.rc = nil
		return err
	}
	return nil
}

// ---------- serving file bytes ----------

func serveBytes(w http.ResponseWriter, r *http.Request, name string, mod time.Time, rs io.ReadSeeker) {
	h := w.Header()
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(name))); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	disposition := "inline"
	if r.URL.Query().Has("download") {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	// Files are user content: never let an uploaded .html run as this site.
	h.Set("Content-Security-Policy", "sandbox")
	http.ServeContent(w, r, name, mod, rs)
}

// serveTarget streams a file or a file inside a zip. Returns false if t is not
// a file.
func (s *server) serveTarget(w http.ResponseWriter, r *http.Request, t *target) bool {
	if t.isDir {
		return false
	}
	if !t.inZip {
		f, err := os.Open(t.fsPath)
		if err != nil {
			http.NotFound(w, r)
			return true
		}
		defer f.Close()
		serveBytes(w, r, filepath.Base(t.fsPath), t.mod, f)
		return true
	}
	a, err := s.zips.open(t.fsPath)
	if err != nil {
		return false
	}
	defer s.zips.release(a)
	zf := a.files[t.inner]
	if zf == nil {
		return false
	}
	name := path.Base(t.inner)
	size := int64(zf.UncompressedSize64)
	if zf.Method == zip.Store {
		off, err := zf.DataOffset()
		if err != nil {
			http.Error(w, "unreadable zip entry", http.StatusInternalServerError)
			return true
		}
		serveBytes(w, r, name, zf.Modified, io.NewSectionReader(a.f, off, size))
		return true
	}
	is := &inflateSeeker{zf: zf, size: size}
	defer is.Close()
	serveBytes(w, r, name, zf.Modified, is)
	return true
}

// ---------- browsing ----------

func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanRel(r.PathValue("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	t, err := s.resolve(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	wantDir := strings.HasSuffix(r.URL.Path, "/")

	var entries []entry
	switch {
	case t.isDir:
		if !wantDir {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusFound)
			return
		}
		des, err := os.ReadDir(t.fsPath)
		if err != nil {
			http.Error(w, "cannot read folder", http.StatusInternalServerError)
			return
		}
		for _, de := range des {
			if strings.HasPrefix(de.Name(), ".") {
				continue
			}
			fi, err := os.Stat(filepath.Join(t.fsPath, de.Name()))
			if err != nil {
				continue
			}
			e := entry{Name: de.Name(), IsDir: fi.IsDir(), Deletable: true}
			if !e.IsDir {
				e.Size = fi.Size()
			}
			entries = append(entries, e)
		}
	case !t.inZip && isZip(t.fsPath) && wantDir:
		a, err := s.zips.open(t.fsPath)
		if err != nil {
			http.Error(w, "not a readable zip: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		entries = a.children("")
		s.zips.release(a)
	case t.inZip:
		a, err := s.zips.open(t.fsPath)
		if err != nil {
			http.Error(w, "not a readable zip: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		defer s.zips.release(a)
		if a.files[t.inner] != nil {
			s.serveTarget(w, r, t)
			return
		}
		if !a.dirs[t.inner] {
			http.NotFound(w, r)
			return
		}
		if !wantDir {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusFound)
			return
		}
		entries = a.children(t.inner)
	default:
		s.serveTarget(w, r, t)
		return
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	for i := range entries {
		e := &entries[i]
		child := e.Name
		if rel != "" {
			child = rel + "/" + e.Name
		}
		e.Href = "/files/" + escapePath(child)
		if e.IsDir {
			e.Href += "/"
			continue
		}
		e.IsZip = isZip(e.Name) && !t.inZip
		e.Share = s.shares.url(s.cfg.PublicURL, child)
	}

	type crumb struct{ Name, Href string }
	crumbs := []crumb{{"files", "/files/"}}
	acc := ""
	if rel != "" {
		for _, p := range strings.Split(rel, "/") {
			acc += p + "/"
			crumbs = append(crumbs, crumb{p, "/files/" + escapePath(strings.TrimSuffix(acc, "/")) + "/"})
		}
	}
	s.render(w, "browse", map[string]any{
		"Rel": rel, "Crumbs": crumbs, "Entries": entries, "Writable": t.isDir,
	})
}

func escapePath(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ---------- share link for a path ----------

func (s *server) isFile(rel string) bool {
	t, err := s.resolve(rel)
	if err != nil || t.isDir {
		return false
	}
	if !t.inZip {
		return true
	}
	a, err := s.zips.open(t.fsPath)
	if err != nil {
		return false
	}
	defer s.zips.release(a)
	return a.files[t.inner] != nil
}

func (s *server) handleLink(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanRel(r.URL.Query().Get("path"))
	if err != nil || !s.isFile(rel) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such file"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": rel, "url": s.shares.url(s.cfg.PublicURL, rel)})
}

// ---------- changes: upload, new folder, delete ----------

func (s *server) realDir(rel string) (string, error) {
	t, err := s.resolve(rel)
	if err != nil || !t.isDir {
		return "", errNotFound
	}
	return t.fsPath, nil
}

// cleanName validates a single file or folder name from a user.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "/" || name == "." || name == ".." || strings.HasPrefix(name, ".") ||
		strings.ContainsAny(name, "\x00") || len(name) > 255 {
		return "", errors.New("invalid name")
	}
	return name, nil
}

// moveToFreeName moves file tmp to dir/name, or dir/name (2).ext etc. if that
// is taken. It claims the name with a hard link, which fails if the name
// exists, so two writers finishing at once can never overwrite each other.
func moveToFreeName(tmp, dir, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 10000; i++ {
		p := filepath.Join(dir, name)
		if i > 1 {
			p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}
		err := os.Link(tmp, p)
		if err == nil {
			os.Remove(tmp)
			return p, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("too many files with that name")
}

func (s *server) relOf(fsPath string) string {
	r, _ := filepath.Rel(s.root, fsPath)
	return filepath.ToSlash(r)
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	rel, err := cleanRel(r.URL.Query().Get("dir"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad folder"})
		return
	}
	dir, err := s.realDir(rel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such folder"})
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected a multipart upload"})
		return
	}
	type saved struct{ Name, URL string }
	var out []saved
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "saved": out})
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			continue
		}
		name, err := cleanName(part.FileName())
		if err != nil {
			continue
		}
		dest, err := writeAtomically(dir, name, part)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "saved": out})
			return
		}
		rel := s.relOf(dest)
		log.Printf("upload: %s", rel)
		out = append(out, saved{filepath.Base(dest), s.shares.url(s.cfg.PublicURL, rel)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": out})
}

// writeAtomically streams src to a hidden temp file in dir, then renames it to
// a free name, so half-written files never show up or get shared.
func writeAtomically(dir, name string, src io.Reader) (string, error) {
	tmp, err := os.CreateTemp(dir, "."+name+".*.part")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(tmp, src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	os.Chmod(tmp.Name(), 0o644)
	dest, err := moveToFreeName(tmp.Name(), dir, name)
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return dest, nil
}

func (s *server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct{ Dir, Name string }
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	rel, err := cleanRel(req.Dir)
	name, nerr := cleanName(req.Name)
	if err != nil || nerr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid folder name"})
		return
	}
	dir, err := s.realDir(rel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such folder"})
		return
	}
	if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "could not create folder (exists?)"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "created"})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	rel, err := cleanRel(req.Path)
	if err != nil || rel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refusing to delete that"})
		return
	}
	t, err := s.resolve(rel)
	if err != nil || t.inZip {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such file or folder"})
		return
	}
	// Remove the link, not what a symlink points to.
	p := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.RemoveAll(p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("delete: %s", rel)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}
