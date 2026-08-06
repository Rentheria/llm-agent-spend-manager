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
	cacheTTL      time.Duration
	cacheMu       sync.Mutex
	cacheCond     *sync.Cond
	cacheSnapshot aggregate.Snapshot
	cacheErr      error
	cacheAt       time.Time
	cacheLoading  bool
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

// WithCacheTTL sets how long a load() result is reused before rescanning
// (default 10s). A non-positive value disables the cache (rescan every request).
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
		load:     load,
		now:      time.Now,
		loc:      time.Local,
		mux:      http.NewServeMux(),
		cacheTTL: defaultCacheTTL,
	}
	s.cacheCond = sync.NewCond(&s.cacheMu)
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
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		// A slow or hung client can't tie up the handler indefinitely (L-01).
		// WriteTimeout is generous because a cold load walks several on-disk
		// stores; the cache keeps the common case well under it.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return srv.ListenAndServe()
}

// cachedLoad returns the current snapshot, reusing a result younger than
// cacheTTL and coalescing concurrent misses into a single load (single-flight),
// so a burst of API requests can't each trigger a full on-disk rescan (L-01).
func (s *Server) cachedLoad() (aggregate.Snapshot, error) {
	s.cacheMu.Lock()
	if s.fresh() {
		defer s.cacheMu.Unlock()
		return s.cacheSnapshot, s.cacheErr
	}
	// Another goroutine is already loading: wait and reuse its result instead of
	// launching a duplicate scan.
	for s.cacheLoading {
		s.cacheCond.Wait()
		if s.fresh() {
			defer s.cacheMu.Unlock()
			return s.cacheSnapshot, s.cacheErr
		}
	}
	s.cacheLoading = true
	s.cacheMu.Unlock()

	snapshot, err := s.load()

	s.cacheMu.Lock()
	s.cacheSnapshot, s.cacheErr, s.cacheAt = snapshot, err, s.now()
	s.cacheLoading = false
	s.cacheCond.Broadcast()
	s.cacheMu.Unlock()
	return snapshot, err
}

// fresh reports whether the cache holds a result still within the TTL. Caller
// holds cacheMu. A non-positive TTL disables caching (never fresh).
func (s *Server) fresh() bool {
	return s.cacheTTL > 0 && !s.cacheAt.IsZero() && s.now().Sub(s.cacheAt) < s.cacheTTL
}

// summaryResponse is the payload of /api/summary for one time window.
type summaryResponse struct {
	Window      string                  `json:"window"`
	GeneratedAt time.Time               `json:"generatedAt"`
	CostLabel   string                  `json:"costLabel"`
	Disclaimer  string                  `json:"disclaimer"`
	ByAgent     []aggregate.AgentTotals `json:"byAgent"`
	ByMode      []aggregate.ModeTotals  `json:"byMode"`
	Grand       aggregate.Totals        `json:"grand"`
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

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.cachedLoad()
	if err != nil {
		http.Error(w, "failed to load usage data", http.StatusInternalServerError)
		return
	}
	window := parseWindow(r.URL.Query().Get("window"))
	now := s.now()
	filtered := aggregate.FilterWindow(snapshot.Records, window, now)
	writeJSON(w, summaryResponse{
		Window:      string(window),
		GeneratedAt: now,
		CostLabel:   aggregate.CostLabel,
		Disclaimer:  aggregate.CostDisclaimer,
		ByAgent:     aggregate.ByAgent(filtered),
		ByMode:      aggregate.ByMode(filtered),
		Grand:       aggregate.Grand(filtered),
	})
}

func (s *Server) handleDaily(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.cachedLoad()
	if err != nil {
		http.Error(w, "failed to load usage data", http.StatusInternalServerError)
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
	snapshot, err := s.cachedLoad()
	if err != nil {
		http.Error(w, "failed to load usage data", http.StatusInternalServerError)
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
	snapshot, err := s.cachedLoad()
	if err != nil {
		http.Error(w, "failed to load usage data", http.StatusInternalServerError)
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
	snapshot, err := s.cachedLoad()
	if err != nil {
		http.Error(w, "failed to load usage data", http.StatusInternalServerError)
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
