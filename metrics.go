package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- Prometheus metrics ----------
//
// Served on CDN_METRICS_ADDR (e.g. 127.0.0.1:9466), never on CDN_ADDR, which
// nginx exposes. Unset, nothing listens; counting in memory is cheap either way.

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type reqKey struct {
	pattern string
	code    int
}

type reqStats struct {
	count   uint64
	sum     float64
	bytes   uint64
	buckets []uint64
}

type metrics struct {
	mu    sync.Mutex
	reqs  map[reqKey]*reqStats
	start time.Time
}

func newMetrics() *metrics {
	return &metrics{reqs: map[reqKey]*reqStats{}, start: time.Now()}
}

// wrap counts each request by the mux pattern it matched (GET /share/{code}),
// so share codes and file paths never become label values.
func (m *metrics) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cw := &countingWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(cw, r)
		pattern := r.Pattern // set by ServeMux on this same request
		if pattern == "" {
			pattern = "unmatched"
		}
		m.observe(reqKey{pattern, cw.code}, time.Since(start).Seconds(), cw.n)
	})
}

func (m *metrics) observe(k reqKey, secs float64, n uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.reqs[k]
	if st == nil {
		st = &reqStats{buckets: make([]uint64, len(durationBuckets))}
		m.reqs[k] = st
	}
	st.count++
	st.sum += secs
	st.bytes += n
	for i, le := range durationBuckets {
		if secs <= le {
			st.buckets[i]++
		}
	}
}

func (m *metrics) write(w io.Writer, s *server) {
	m.mu.Lock()
	keys := make([]reqKey, 0, len(m.reqs))
	for k := range m.reqs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pattern != keys[j].pattern {
			return keys[i].pattern < keys[j].pattern
		}
		return keys[i].code < keys[j].code
	})
	lbl := func(k reqKey) string { return fmt.Sprintf(`pattern=%q,code="%d"`, k.pattern, k.code) }

	fmt.Fprintln(w, "# HELP cdn_http_requests_total Requests by mux pattern and status code.")
	fmt.Fprintln(w, "# TYPE cdn_http_requests_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "cdn_http_requests_total{%s} %d\n", lbl(k), m.reqs[k].count)
	}
	fmt.Fprintln(w, "# HELP cdn_http_response_bytes_total Body bytes sent, by mux pattern and status code.")
	fmt.Fprintln(w, "# TYPE cdn_http_response_bytes_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "cdn_http_response_bytes_total{%s} %d\n", lbl(k), m.reqs[k].bytes)
	}
	fmt.Fprintln(w, "# HELP cdn_http_request_duration_seconds Time to serve the whole response.")
	fmt.Fprintln(w, "# TYPE cdn_http_request_duration_seconds histogram")
	for _, k := range keys {
		st := m.reqs[k]
		for i, le := range durationBuckets {
			fmt.Fprintf(w, "cdn_http_request_duration_seconds_bucket{%s,le=\"%g\"} %d\n", lbl(k), le, st.buckets[i])
		}
		fmt.Fprintf(w, "cdn_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", lbl(k), st.count)
		fmt.Fprintf(w, "cdn_http_request_duration_seconds_sum{%s} %g\n", lbl(k), st.sum)
		fmt.Fprintf(w, "cdn_http_request_duration_seconds_count{%s} %d\n", lbl(k), st.count)
	}
	m.mu.Unlock()

	if s != nil && s.fetcher != nil {
		byStatus := map[string]int{"queued": 0, "downloading": 0, "converting": 0, "done": 0, "error": 0}
		s.fetcher.mu.Lock()
		for _, j := range s.fetcher.jobs {
			byStatus[j.Status]++
		}
		s.fetcher.mu.Unlock()
		statuses := make([]string, 0, len(byStatus))
		for st := range byStatus {
			statuses = append(statuses, st)
		}
		sort.Strings(statuses)
		fmt.Fprintln(w, "# HELP cdn_fetch_jobs Fetch jobs kept in memory, by status.")
		fmt.Fprintln(w, "# TYPE cdn_fetch_jobs gauge")
		for _, st := range statuses {
			fmt.Fprintf(w, "cdn_fetch_jobs{status=%q} %d\n", st, byStatus[st])
		}
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	fmt.Fprintf(w, "# TYPE go_memstats_sys_bytes gauge\ngo_memstats_sys_bytes %d\n", ms.Sys)
	fmt.Fprintf(w, "# TYPE go_gc_cycles_total counter\ngo_gc_cycles_total %d\n", ms.NumGC)
	fmt.Fprintf(w, "# TYPE process_start_time_seconds gauge\nprocess_start_time_seconds %d\n", m.start.Unix())
}

// serveMetrics listens on addr (meant to be loopback) and serves only /metrics.
func (m *metrics) serve(addr string, s *server) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var b strings.Builder
		m.write(&b, s)
		io.WriteString(w, b.String())
	})
	go func() {
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		if err := srv.Serve(ln); err != nil {
			fmt.Println("metrics listener:", err)
		}
	}()
	return ln, nil
}

// countingWriter records the status and body size. It keeps io.ReaderFrom
// (sendfile for http.ServeContent) and Flush working, and Unwrap lets
// http.ResponseController reach the real writer.
type countingWriter struct {
	http.ResponseWriter
	code        int
	n           uint64
	wroteHeader bool
}

func (c *countingWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.code, c.wroteHeader = code, true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.wroteHeader = true
	n, err := c.ResponseWriter.Write(p)
	c.n += uint64(n)
	return n, err
}

func (c *countingWriter) ReadFrom(r io.Reader) (int64, error) {
	c.wroteHeader = true
	var n int64
	var err error
	if rf, ok := c.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(r)
	} else {
		n, err = io.Copy(struct{ io.Writer }{c.ResponseWriter}, r)
	}
	c.n += uint64(n)
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
