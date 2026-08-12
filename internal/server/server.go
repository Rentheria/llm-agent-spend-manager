// Package server exposes the T5 aggregation over local HTTP: a small JSON API
// plus the embedded dashboard (web/, compiled into the binary via go:embed so
// there are no runtime file dependencies).
//
// Binding is the caller's choice (see Serve); the CLI listens on loopback by
// default and only exposes the LAN with `serve --lan`, which also requires an
// access token (see WithToken and architecture.md §4.3).
//
// Every $ the API emits is an estimated-equivalent cost, never money charged —
// the JSON carries CostLabel/CostDisclaimer so any client renders the right
// wording (see internal/aggregate and docs/architecture.md §3.1). The primary
// metric is quota (T80 / T81): /api/quota reuses quota.Analyze, the same path
// the CLI command takes.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/advise"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/bootfiles"
	"github.com/Rentheria/llm-agent-spend-manager/internal/outcome"
	"github.com/Rentheria/llm-agent-spend-manager/internal/quota"
)

//go:embed all:web
var webFS embed.FS

func init() {
	// Go doesn't know .webmanifest out of the box; without this the PWA manifest
	// (T18) is served as octet-stream. .js is already registered (needed so the
	// service worker loads with a JavaScript MIME type).
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// SnapshotLoader returns the current machine snapshot (normalized turns plus
// quota signals). Its result is cached for a short TTL and shared across
// concurrent requests (see cachedLoad). In production this is
// aggregate.CollectSnapshot(homeDir); tests inject a fixture loader.
type SnapshotLoader func() (aggregate.Snapshot, error)

// ChangesLoader returns the marked fleet changes the outcome ledger contrasts
// metrics against (git commits + fleet log). Nil means /api/outcome is not
// wired — the route then answers 501 rather than inventing an empty ledger.
type ChangesLoader func(ctx context.Context) (outcome.ChangeLedger, error)

// BootFilesLoader measures the shared files every agent reads at session start.
// Nil means /api/advise simply omits that section — unlike the outcome ledger
// this is an extra measurement on an otherwise complete report, not the whole
// answer, so a missing loader is not worth failing the request over.
//
// It returns only the report, never the new snapshot: the dashboard must not
// write it. Its systemd unit runs with ProtectHome=read-only
// (docs/servicios-permanentes.md), and advancing the delta's reference point
// because someone refreshed a browser tab would flatten the delta the CLI
// reports to noise.
type BootFilesLoader func() (bootfiles.Report, error)

// authCookieName holds the access token once a client has presented it via the
// QR's ?token= query, so the dashboard's later /api fetches authenticate
// without carrying the token in every URL.
const authCookieName = "lasm_token"

// defaultCacheTTL bounds how long a load() result is reused before rescanning.
// Short enough that the dashboard stays fresh, long enough that a burst of
// requests (or a peer hammering /api/*) can't force a full disk rescan per hit.
const defaultCacheTTL = 10 * time.Second

// defaultWarmupWait bounds how long a request may wait when no snapshot exists
// yet — the window between start-up and the first scan finishing. Past it the
// request is answered 503 + Retry-After instead of holding the connection open
// for the length of a full scan, which on this machine outlives WriteTimeout
// and gets answered with nothing at all (T127/F1). Serve primes the cache at
// start-up, so in a long-running service this window is the first seconds of
// the process and nothing else.
const defaultWarmupWait = 5 * time.Second

// errWarmingUp means no snapshot has been read yet and the first scan is still
// running. It is a "come back in a moment", not a failure, and handlers map it
// to 503 so a client can tell it apart from data that genuinely is zero.
var errWarmingUp = errors.New("usage snapshot is still being read from disk")

// defaultBreakdownDays is how far back /api/quota's "who is eating it" looks,
// matching the CLI quota command's default.
const defaultBreakdownDays = 3

// Server wires the JSON API and the embedded dashboard onto one mux.
type Server struct {
	load        SnapshotLoader
	changesLoad ChangesLoader
	bootLoad    BootFilesLoader
	getenv      func(string) string
	now         func() time.Time
	loc         *time.Location
	mux         *http.ServeMux
	handler     http.Handler // mux, wrapped with token auth when token != ""
	token       string

	// Result cache with single-flight: coalesces concurrent loads and reuses a
	// fresh result for cacheTTL, so each request no longer triggers a full
	// re-scan of every agent's on-disk data (L-01).
	//
	// Once any snapshot is held, a request is never blocked on a rescan: the
	// held copy is served immediately and the refresh runs in the background
	// (see cachedLoad). A full scan of this machine's stores takes tens of
	// seconds — longer than the server's WriteTimeout — so a handler that
	// waited for one had its connection closed with no body written at all,
	// which the dashboard renders as an empty page (T127/F1).
	cacheTTL      time.Duration
	warmupWait    time.Duration
	cacheMu       sync.Mutex
	cacheSnapshot aggregate.Snapshot
	cacheErr      error
	// cacheAt is when the held snapshot was read off disk; lastAttempt is when a
	// load last finished, successfully or not. The two diverge after a failed
	// refresh, which keeps the good snapshot but still counts as an attempt so
	// an unreadable store can't spin the scanner in a hot loop.
	cacheAt     time.Time
	lastAttempt time.Time
	// cacheReady is false only during warm-up, before any load has finished. It
	// is what separates "no answer yet" from "an answer that happens to be an
	// error", which are different HTTP responses.
	cacheReady bool
	// loading is non-nil while a scan is in flight and closed when it finishes,
	// so a warm-up waiter can block on it with a deadline (sync.Cond cannot).
	loading chan struct{}
}

// Option customizes a Server (clock/location injection for tests).
type Option func(*Server)

// WithClock overrides the time source (default time.Now).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithLocation overrides the timezone used for day/window bucketing
// (default time.Local).
func WithLocation(loc *time.Location) Option { return func(s *Server) { s.loc = loc } }

// WithToken requires this access token on every request (dashboard and API).
// An empty token — the default, used for loopback binds — disables auth. The
// CLI only sets a non-empty token when exposing the server to the LAN (--lan),
// so a peer on the same Wi-Fi can no longer read the dashboard unauthenticated.
func WithToken(token string) Option { return func(s *Server) { s.token = token } }

// WithCacheTTL sets how long a load() result is reused before a rescan is due
// (default 10s). A non-positive value makes every request schedule a rescan.
//
// The TTL governs when a refresh starts, not whether a request waits for one:
// past the TTL the held snapshot is still served immediately and the rescan
// runs behind it (see cachedLoad).
func WithCacheTTL(ttl time.Duration) Option { return func(s *Server) { s.cacheTTL = ttl } }

// WithChangesLoader wires /api/outcome to an I/O source for marked changes.
// Without it the route answers 501 — an empty ledger would look like "nothing
// happened", which is a different claim from "we didn't look".
func WithChangesLoader(load ChangesLoader) Option {
	return func(s *Server) { s.changesLoad = load }
}

// WithBootFilesLoader wires the boot-file weight into /api/advise.
func WithBootFilesLoader(load BootFilesLoader) Option {
	return func(s *Server) { s.bootLoad = load }
}

// WithGetenv overrides the environment reader used for quota plan config
// (default os.Getenv via quota.LoadConfig's nil-fallback). Tests inject a map.
func WithGetenv(getenv func(string) string) Option {
	return func(s *Server) { s.getenv = getenv }
}

// New builds a Server that reads the machine snapshot via load.
func New(load SnapshotLoader, opts ...Option) *Server {
	s := &Server{
		load:       load,
		now:        time.Now,
		loc:        time.Local,
		mux:        http.NewServeMux(),
		cacheTTL:   defaultCacheTTL,
		warmupWait: defaultWarmupWait,
	}
	for _, o := range opts {
		o(s)
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		// web/ is embedded at build time; its absence is a build bug, not a
		// runtime condition.
		panic(err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("/api/summary", s.handleSummary)
	s.mux.HandleFunc("/api/daily", s.handleDaily)
	s.mux.HandleFunc("/api/advice", s.handleAdvice)
	s.mux.HandleFunc("/api/quota", s.handleQuota)
	s.mux.HandleFunc("/api/outcome", s.handleOutcome)

	s.handler = s.mux
	if s.token != "" {
		s.handler = s.authMiddleware(s.mux)
	}
	return s
}

// Handler returns the HTTP handler (JSON API + dashboard), token-guarded when a
// token was set via WithToken.
func (s *Server) Handler() http.Handler { return s.handler }

// authMiddleware rejects any request whose token doesn't match, comparing in
// constant time so the check can't be timed. A valid token presented in the URL
// query (the QR case) is stored in an HttpOnly cookie so the phone's subsequent
// /api fetches stay authenticated without the token in every link.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	expected := []byte(s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, fromQuery := extractToken(r)
		if subtle.ConstantTimeCompare([]byte(provided), expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="llm-agent-spend-manager"`)
			http.Error(w, "unauthorized: falta o no coincide el token de acceso (--lan)", http.StatusUnauthorized)
			return
		}
		if fromQuery {
			http.SetCookie(w, &http.Cookie{
				Name:     authCookieName,
				Value:    provided,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// extractToken pulls the access token from (in order) the ?token= query param,
// the X-Auth-Token header, an Authorization: Bearer header, or the auth cookie.
// fromQuery is true only for the query-param case, which is the one worth
// persisting as a cookie (a scanned QR lands the token in the URL exactly once).
func extractToken(r *http.Request) (token string, fromQuery bool) {
	if q := r.URL.Query().Get("token"); q != "" {
		return q, true
	}
	if h := r.Header.Get("X-Auth-Token"); h != "" {
		return h, false
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer "), false
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		return c.Value, false
	}
	return "", false
}

// Serve starts an HTTP server on addr and blocks. addr follows net/http
// conventions (e.g. ":4600" for all interfaces, "127.0.0.1:4600" for localhost
// only).
func (s *Server) Serve(addr string) error {
	// Read the machine once before anyone asks, so the first page load is
	// answered from the cache instead of paying for a cold scan (T127/F1).
	s.Warm()
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		// A slow or hung client can't tie up the handler indefinitely (L-01).
		// No handler waits on a scan any more — the longest a request can block
		// on data is warmupWait — so this bounds slow clients only, and a cold
		// load can no longer run past it and lose the response (T127/F1).
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return srv.ListenAndServe()
}

// cachedLoad returns the snapshot a request should be answered from, plus the
// moment it was read off disk (which is older than "now" whenever a background
// refresh is still running).
//
// Once a snapshot is held this never blocks: a stale copy is returned at once
// and the rescan happens in the background (stale-while-revalidate), coalesced
// into a single load so a burst of requests can't each trigger a rescan (L-01).
// Only warm-up can wait, and only for warmupWait.
//
// Blocking a request for the length of a full scan is precisely what emptied
// the dashboard (T127/F1): the scan outlived WriteTimeout, so the connection
// was closed before a single byte of the response was written and every panel
// fell back to its empty state.
func (s *Server) cachedLoad(ctx context.Context) (aggregate.Snapshot, time.Time, error) {
	s.cacheMu.Lock()
	if !s.fresh() || !s.cacheReady {
		s.startLoadLocked()
	}
	if s.cacheReady {
		snapshot, at, err := s.cacheSnapshot, s.cacheAt, s.cacheErr
		s.cacheMu.Unlock()
		return snapshot, at, err
	}
	done := s.loading
	s.cacheMu.Unlock()

	// Warm-up: give the first scan a bounded moment to land, then answer "not
	// yet" rather than holding the connection until it dies.
	timer := time.NewTimer(s.warmupWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
		return aggregate.Snapshot{}, time.Time{}, ctx.Err()
	}

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !s.cacheReady {
		return aggregate.Snapshot{}, time.Time{}, errWarmingUp
	}
	return s.cacheSnapshot, s.cacheAt, s.cacheErr
}

// startLoadLocked launches a scan unless one is already running. Caller holds
// cacheMu. The scan itself runs without the lock held, so requests keep being
// served from the held snapshot for its whole duration.
func (s *Server) startLoadLocked() {
	if s.loading != nil {
		return
	}
	done := make(chan struct{})
	s.loading = done
	go func() {
		defer close(done)
		snapshot, err := s.load()

		s.cacheMu.Lock()
		defer s.cacheMu.Unlock()
		s.loading = nil
		s.lastAttempt = s.now()
		// A failed rescan must not erase a snapshot that worked. Dropping to an
		// error page because one read of a live SQLite store happened to fail
		// would turn a transient blip into "the fleet did nothing" — the same
		// wrong claim this whole path exists to avoid.
		if err != nil && s.cacheReady && s.cacheErr == nil {
			return
		}
		s.cacheSnapshot, s.cacheErr, s.cacheAt = snapshot, err, s.lastAttempt
		s.cacheReady = true
	}()
}

// Warm starts the first scan in the background so the cache is primed before
// any request arrives. Serve calls it; a long-running service therefore pays
// the cold-scan cost once at start-up instead of on a user's first page load.
func (s *Server) Warm() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.startLoadLocked()
}

// fresh reports whether the last load attempt is recent enough that another one
// isn't due yet. Caller holds cacheMu. A non-positive TTL disables caching, so
// every request schedules a refresh (it still answers from the held snapshot).
func (s *Server) fresh() bool {
	return s.cacheTTL > 0 && !s.lastAttempt.IsZero() && s.now().Sub(s.lastAttempt) < s.cacheTTL
}

// summaryResponse is the payload of /api/summary for one time window.
type summaryResponse struct {
	Window      string    `json:"window"`
	GeneratedAt time.Time `json:"generatedAt"`
	// SnapshotAt is when the underlying scan read the disk; GeneratedAt is when
	// this response was rendered. They differ while a refresh is in flight, and
	// the gap matters: a snapshot taken yesterday, filtered to the "today"
	// window, reports zero — which would read as "the fleet did nothing today"
	// rather than "these numbers are from yesterday" (T127/F1).
	SnapshotAt time.Time               `json:"snapshotAt"`
	CostLabel  string                  `json:"costLabel"`
	Disclaimer string                  `json:"disclaimer"`
	ByAgent    []aggregate.AgentTotals `json:"byAgent"`
	ByMode     []aggregate.ModeTotals  `json:"byMode"`
	Grand      aggregate.Totals        `json:"grand"`
}

// dailyResponse is the payload of /api/daily: per-agent, per-day totals over
// all history (what the dashboard table renders).
type dailyResponse struct {
	GeneratedAt time.Time                  `json:"generatedAt"`
	CostLabel   string                     `json:"costLabel"`
	Disclaimer  string                     `json:"disclaimer"`
	ByAgentDay  []aggregate.AgentDayTotals `json:"byAgentDay"`
}

// outcomeResponse is the payload of /api/outcome: marked changes plus the
// level-shift ledger, the same shape the CLI emits with --json.
type outcomeResponse struct {
	GeneratedAt time.Time            `json:"generatedAt"`
	Window      string               `json:"window"`
	CostLabel   string               `json:"costLabel"`
	Disclaimer  string               `json:"disclaimer"`
	Changes     outcome.ChangeLedger `json:"changes"`
	Outcomes    advise.OutcomeLedger `json:"outcomes"`
}

// snapshotHeader names the moment the served data was read off disk. A client
// that cares whether it is looking at a refresh-in-flight (and every time-windowed
// view does — a stale snapshot filtered to "today" reports zero, which is a
// different claim from "nothing ran today") can read it without parsing a body.
const snapshotHeader = "X-Snapshot-At"

// loadForRequest resolves the snapshot for one request. When it returns false
// the response has already been written and the handler must return: 503 while
// the first scan is still running, 500 for a genuine read failure. On success
// it stamps the snapshot's age onto the response.
// The snapshot's timestamp is returned alongside the data it belongs to, never
// re-read afterwards: a background refresh landing in between would stamp a
// response with a time that doesn't describe the numbers in it.
func (s *Server) loadForRequest(w http.ResponseWriter, r *http.Request) (aggregate.Snapshot, time.Time, bool) {
	snapshot, at, err := s.cachedLoad(r.Context())
	switch {
	case errors.Is(err, errWarmingUp):
		w.Header().Set("Retry-After", "5")
		http.Error(w, "usage data is still being read from disk; retry shortly", http.StatusServiceUnavailable)
		return aggregate.Snapshot{}, time.Time{}, false
	case err != nil:
		http.Error(w, "failed to load usage data", http.StatusInternalServerError)
		return aggregate.Snapshot{}, time.Time{}, false
	}
	w.Header().Set(snapshotHeader, at.Format(time.RFC3339))
	return snapshot, at, true
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	snapshot, snapshotAt, ok := s.loadForRequest(w, r)
	if !ok {
		return
	}
	window := parseWindow(r.URL.Query().Get("window"))
	now := s.now()
	filtered := aggregate.FilterWindow(snapshot.Records, window, now)
	writeJSON(w, summaryResponse{
		Window:      string(window),
		GeneratedAt: now,
		SnapshotAt:  snapshotAt,
		CostLabel:   aggregate.CostLabel,
		Disclaimer:  aggregate.CostDisclaimer,
		ByAgent:     aggregate.ByAgent(filtered),
		ByMode:      aggregate.ByMode(filtered),
		Grand:       aggregate.Grand(filtered),
	})
}

func (s *Server) handleDaily(w http.ResponseWriter, r *http.Request) {
	snapshot, _, ok := s.loadForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, dailyResponse{
		GeneratedAt: s.now(),
		CostLabel:   aggregate.CostLabel,
		Disclaimer:  aggregate.CostDisclaimer,
		ByAgentDay:  aggregate.ByAgentDay(snapshot.Records, s.loc),
	})
}

// handleAdvice serves the self-improvement report (internal/advise): per-bucket
// cost attribution, the efficiency trend, and the derived findings. It defaults
// to the whole history rather than today, because a trend needs several days of
// data and "today" would almost always answer insufficient-data.
func (s *Server) handleAdvice(w http.ResponseWriter, r *http.Request) {
	snapshot, _, ok := s.loadForRequest(w, r)
	if !ok {
		return
	}
	window := aggregate.WindowAll
	if requested := r.URL.Query().Get("window"); requested != "" {
		window = parseWindow(requested)
	}
	filtered := aggregate.FilterWindow(snapshot.Records, window, s.now())
	writeJSON(w, s.withBootFiles(advise.Analyze(filtered, string(window), s.loc)))
}

// withBootFiles attaches the boot-file weight when a loader is wired. A failed
// measurement is left off the report instead of failing the request: the cost
// analysis is complete without it, and a section that isn't there is easier to
// read correctly than one showing zeros.
func (s *Server) withBootFiles(report advise.Report) advise.Report {
	if s.bootLoad == nil {
		return report
	}
	boot, err := s.bootLoad()
	if err != nil {
		return report
	}
	return advise.WithBootFiles(report, boot)
}

// handleQuota serves the quota report (internal/quota.Analyze) — the same path
// the CLI `quota` command takes. The primary metric is how much of each cycle is
// gone and how long it lasts; the $ stays secondary under its mandatory label.
func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	snapshot, _, ok := s.loadForRequest(w, r)
	if !ok {
		return
	}
	cfg, err := quota.LoadConfig(s.getenv)
	if err != nil {
		http.Error(w, "invalid plan configuration: "+err.Error(), http.StatusBadRequest)
		return
	}
	days := defaultBreakdownDays
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			http.Error(w, "days must be a non-negative integer", http.StatusBadRequest)
			return
		}
		days = parsed
	}
	now := s.now()
	writeJSON(w, quota.Analyze(snapshot, cfg, breakdownSince(now, days), now))
}

// handleOutcome serves the outcome ledger: marked changes (CollectChanges) plus
// level shifts with attribution (BuildOutcomeLedger → Attribute). Defaults to
// all history, same as the CLI, because a level shift needs days on both sides.
func (s *Server) handleOutcome(w http.ResponseWriter, r *http.Request) {
	if s.changesLoad == nil {
		http.Error(w, "outcome not configured: no changes loader wired", http.StatusNotImplemented)
		return
	}
	snapshot, _, ok := s.loadForRequest(w, r)
	if !ok {
		return
	}
	changes, err := s.changesLoad(r.Context())
	if err != nil {
		http.Error(w, "failed to read marked changes", http.StatusInternalServerError)
		return
	}
	window := aggregate.WindowAll
	if requested := r.URL.Query().Get("window"); requested != "" {
		window = parseWindow(requested)
	}
	now := s.now()
	filtered := aggregate.FilterWindow(snapshot.Records, window, now)
	report := advise.Analyze(filtered, string(window), s.loc)
	writeJSON(w, outcomeResponse{
		GeneratedAt: now,
		Window:      string(window),
		CostLabel:   aggregate.CostLabel,
		Disclaimer:  aggregate.CostDisclaimer,
		Changes:     changes,
		Outcomes:    advise.BuildOutcomeLedger(filtered, report, changes.Changes, s.loc),
	})
}

// breakdownSince turns a day count into the lower bound of the quota breakdown
// period. Zero days means all history, expressed as the zero time — same helper
// the CLI quota command uses.
func breakdownSince(now time.Time, days int) time.Time {
	if days == 0 {
		return time.Time{}
	}
	return now.AddDate(0, 0, -days)
}

// parseWindow maps the query value to a Window, defaulting to today for any
// empty/unknown value.
func parseWindow(v string) aggregate.Window {
	switch aggregate.Window(v) {
	case aggregate.WindowWeek:
		return aggregate.WindowWeek
	case aggregate.WindowAll:
		return aggregate.WindowAll
	default:
		return aggregate.WindowToday
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
