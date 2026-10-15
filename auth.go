package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "cdn_session"
	sessionTTL    = 30 * 24 * time.Hour
	pbkdf2Iter    = 600_000
)

// ---------- password hashing ----------
// Format: pbkdf2-sha256:<iterations>:<salt b64>:<key b64>. No '$', so it is safe
// in a systemd EnvironmentFile.

type passwordHash struct {
	iter      int
	salt, key []byte
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("pbkdf2-sha256:%d:%s:%s", pbkdf2Iter, b64(salt), b64(key)), nil
}

func parsePasswordHash(s string) (*passwordHash, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return nil, errors.New("not a pbkdf2-sha256 hash from `private-cdn hash-password`")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return nil, errors.New("bad iteration count")
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	key, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(key) == 0 {
		return nil, errors.New("bad salt or key encoding")
	}
	return &passwordHash{iter, salt, key}, nil
}

func checkPassword(encoded, pw string) bool {
	h, err := parsePasswordHash(encoded)
	if err != nil {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, pw, h.salt, h.iter, len(h.key))
	return err == nil && subtle.ConstantTimeCompare(key, h.key) == 1
}

// hashPasswordCmd reads a password from stdin and prints its hash.
func hashPasswordCmd() error {
	fmt.Fprint(os.Stderr, "Password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	pw := strings.TrimRight(line, "\r\n")
	if len(pw) < 8 {
		return errors.New("use at least 8 characters")
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

// ---------- sessions ----------
// Cookie value: base64(user|expiry).base64(hmac). Stateless; changing the
// server secret or the username signs everyone out.

func (s *server) newSession(w http.ResponseWriter) {
	exp := time.Now().Add(sessionTTL)
	payload := base64.RawURLEncoding.EncodeToString([]byte(s.cfg.User + "|" + strconv.FormatInt(exp.Unix(), 10)))
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    payload + "." + s.sign(payload),
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.PublicURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) sign(payload string) string {
	m := hmac.New(sha256.New, s.sessionKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	user, expStr, ok := strings.Cut(string(raw), "|")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	return ok && err == nil && user == s.cfg.User && time.Now().Unix() < exp
}

// sameOrigin rejects cross-site state-changing requests (CSRF), on top of the
// SameSite=Lax cookie.
func (s *server) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == s.cfg.PublicURL
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			if r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "/api/") {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// ---------- login handlers ----------

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/files/"
	}
	return next
}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.loggedIn(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	s.render(w, "login", map[string]any{"Next": r.URL.Query().Get("next")})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := r.FormValue("next")
	ip := clientIP(r)
	if wait := s.limiter.blocked(ip); wait > 0 {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login", map[string]any{"Next": next,
			"Error": fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(wait.Minutes())+1)})
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(r.FormValue("username")), []byte(s.cfg.User)) == 1
	passOK := checkPassword(s.cfg.PasswordHash, r.FormValue("password"))
	if !userOK || !passOK {
		s.limiter.fail(ip)
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", map[string]any{"Next": next, "Error": "Wrong username or password."})
		return
	}
	s.limiter.reset(ip)
	s.newSession(w)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// clientIP returns the address used for the login lockout: X-Real-IP from
// nginx. Proxy headers are only trusted when the request comes from loopback
// (nginx), so a client reaching the port directly can't pick its own address.
// The shipped nginx config blanks CF-Connecting-IP (cdn is DNS-only, so a
// client could forge it); if you put Cloudflare's proxy in front, forward it
// instead.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}
	for _, h := range []string{"CF-Connecting-IP", "X-Real-IP"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return host
}

// ---------- brute-force limiter ----------

type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*attempts
}

type attempts struct {
	fails   int
	first   time.Time
	blocked time.Time
}

const (
	maxFails    = 5
	failWindow  = 15 * time.Minute
	blockPeriod = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter { return &loginLimiter{m: map[string]*attempts{}} }

func (l *loginLimiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.m[ip]; a != nil {
		if d := time.Until(a.blocked); d > 0 {
			return d
		}
	}
	return 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, a := range l.m { // keep the map small
		if now.Sub(a.first) > failWindow && now.After(a.blocked) {
			delete(l.m, k)
		}
	}
	a := l.m[ip]
	if a == nil || now.Sub(a.first) > failWindow {
		a = &attempts{first: now}
		l.m[ip] = a
	}
	a.fails++
	if a.fails >= maxFails {
		a.blocked = now.Add(blockPeriod)
		a.fails = 0
		a.first = now
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}
