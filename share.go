package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"sync"
)

// Share codes are the first 16 base64url characters (96 bits) of
// HMAC-SHA256(shareKey, path). They are stable, so a file keeps its link, and
// unguessable without the key. Codes are derived, not stored as secrets: the
// index only maps code -> path for lookups, and every lookup is re-verified
// against the key, so rotating the secret revokes every link.
const shareCodeLen = 16

type shareStore struct {
	key []byte

	mu     sync.Mutex
	byCode map[string]string
	f      *os.File
}

func openShareStore(path string, key []byte) (*shareStore, error) {
	s := &shareStore{key: key, byCode: map[string]string{}}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var rec struct{ C, P string }
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.C != "" {
				s.byCode[rec.C] = rec.P
			}
		}
		f.Close()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s.f = f
	return s, nil
}

func (s *shareStore) compute(rel string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(rel))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:shareCodeLen]
}

// code returns the share code for rel and remembers it for lookups.
func (s *shareStore) code(rel string) string {
	c := s.compute(rel)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byCode[c] != rel {
		s.byCode[c] = rel
		line, _ := json.Marshal(struct{ C, P string }{c, rel})
		s.f.Write(append(line, '\n'))
	}
	return c
}

func (s *shareStore) url(base, rel string) string { return base + "/share/" + s.code(rel) }

func (s *shareStore) lookup(code string) (string, bool) {
	s.mu.Lock()
	rel, ok := s.byCode[code]
	s.mu.Unlock()
	if !ok || !hmac.Equal([]byte(s.compute(rel)), []byte(code)) {
		return "", false
	}
	return rel, true
}

// handleShare is public: the code is the credential.
func (s *server) handleShare(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.shares.lookup(r.PathValue("code"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, err := s.resolve(rel)
	if err != nil || !s.serveTarget(w, r, t) {
		http.Error(w, "This file no longer exists.", http.StatusNotFound)
		return
	}
}
