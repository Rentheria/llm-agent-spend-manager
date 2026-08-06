package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/advise"
	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/outcome"
	"github.com/Rentheria/llm-agent-spend-manager/internal/quota"
)

func fixedNow() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }

func fixtureLoader() (aggregate.Snapshot, error) {
	now := fixedNow()
	return aggregate.Snapshot{Records: []aggregate.Record{
		{Agent: aggregate.AgentClaudeCode, Timestamp: now.Add(-1 * time.Hour), Model: "claude-opus-4-8", Input: 1000, Output: 200, CostUSD: 0.01, CostKnown: true},
		{Agent: aggregate.AgentOpenClaw, Timestamp: now.Add(-2 * time.Hour), Model: "claude-sonnet-5", Input: 500, Output: 100, CostUSD: 0.002, CostKnown: true},
		{Agent: aggregate.AgentClaudeCode, Timestamp: now.Add(-3 * 24 * time.Hour), Model: "claude-opus-4-8", Input: 300, Output: 50, CostUSD: 0.005, CostKnown: true},
	}}, nil
}

func newTestServer() http.Handler {
	return New(fixtureLoader,
		WithClock(fixedNow),
		WithLocation(time.UTC),
	).Handler()
}

func TestSummary_TodayWindow(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary?window=today", nil)
	newTestServer().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp summaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != "today" {
		t.Errorf("window = %q", resp.Window)
	}
	// Only the two records from today survive the window; the 3-days-ago one drops.
	if resp.Grand.Turns != 2 {
		t.Errorf("today grand turns = %d, want 2", resp.Grand.Turns)
	}
	if len(resp.ByAgent) != 2 {
		t.Errorf("today byAgent = %d, want 2", len(resp.ByAgent))
	}
	if resp.CostLabel != aggregate.CostLabel {
		t.Errorf("costLabel = %q, want %q", resp.CostLabel, aggregate.CostLabel)
	}
	if !strings.Contains(resp.Disclaimer, "NO es gasto real") {
		t.Errorf("disclaimer missing the not-real-spend wording: %q", resp.Disclaimer)
	}
}

func TestSummary_AllWindowSeesEverything(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary?window=all", nil)
	newTestServer().ServeHTTP(rr, req)

	var resp summaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Grand.Turns != 3 {
		t.Errorf("all grand turns = %d, want 3", resp.Grand.Turns)
	}
}

func TestSummary_DefaultsToToday(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	newTestServer().ServeHTTP(rr, req)

	var resp summaryResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Window != "today" {
		t.Errorf("default window = %q, want today", resp.Window)
	}
}

func TestDaily(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/daily", nil)
	newTestServer().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp dailyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 3 records across 2 distinct days for Claude Code (today + 3 days ago) and 1
	// for OpenClaw today. Claude Code: 2 days, OpenClaw: 1 day -> 3 agent-day rows.
	if len(resp.ByAgentDay) != 3 {
		t.Errorf("byAgentDay rows = %d, want 3", len(resp.ByAgentDay))
	}
	// Newest day first.
	if resp.ByAgentDay[0].Day != "2026-07-26" {
		t.Errorf("first row day = %q, want 2026-07-26", resp.ByAgentDay[0].Day)
	}
}

func TestServesDashboardIndex(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	newTestServer().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "llm-agent-spend-manager") {
		t.Errorf("index.html not served: %q", rr.Body.String())
	}
}

// El dashboard tiene que traer la misma dimensión que el CLI. No se puede
// ejecutar su JS aquí, pero sí exigir que la sección y las frases que la hacen
// legible sigan embebidas: si alguien la borra, esto falla en vez de dejar una
// pantalla que se lee completa mientras le falta un techo.
func TestDashboard_CarriesTheContextWindowSection(t *testing.T) {
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()

	for _, want := range []string{
		"Cuánto contexto queda",
		"contextWindowsEl",
		"sin escribir un solo token más",
		"aviso a partir de",
		"no derivable",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("el dashboard no trae %q", want)
		}
	}
}

func TestLoaderErrorIs500(t *testing.T) {
	failing := func() (aggregate.Snapshot, error) { return aggregate.Snapshot{}, errBoom }
	h := New(failing, WithClock(fixedNow), WithLocation(time.UTC)).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

type boomError struct{}

func (boomError) Error() string { return "boom" }

var errBoom = boomError{}

// newTokenServer builds a token-guarded server (the --lan case).
func newTokenServer(token string) http.Handler {
	return New(fixtureLoader,
		WithClock(fixedNow),
		WithLocation(time.UTC),
		WithToken(token),
	).Handler()
}

func TestAuth_NoTokenMeansOpen(t *testing.T) {
	// The default (loopback) server has no token: every route is open.
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("no-token server should serve openly, got %d", rr.Code)
	}
}

func TestAuth_MissingTokenIs401(t *testing.T) {
	h := newTokenServer("good-token")
	for _, path := range []string{"/", "/api/summary", "/api/daily", "/api/quota", "/api/outcome", "/api/advice"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, rr.Code)
		}
	}
}

func TestAuth_WrongTokenIs401(t *testing.T) {
	h := newTokenServer("good-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/summary?token=bad-token", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rr.Code)
	}
}

func TestAuth_ValidTokenViaQueryHeaderAndCookie(t *testing.T) {
	h := newTokenServer("good-token")

	// Query param: authenticates AND sets the cookie for later fetches.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/summary?token=good-token", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("query token = %d, want 200", rr.Code)
	}
	var gotCookie string
	for _, c := range rr.Result().Cookies() {
		if c.Name == authCookieName {
			gotCookie = c.Value
			if !c.HttpOnly {
				t.Error("auth cookie should be HttpOnly")
			}
		}
	}
	if gotCookie != "good-token" {
		t.Errorf("auth cookie = %q, want good-token", gotCookie)
	}

	// X-Auth-Token header.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/daily", nil)
	req.Header.Set("X-Auth-Token", "good-token")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("X-Auth-Token = %d, want 200", rr.Code)
	}

	// Authorization: Bearer header.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/daily", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("Bearer token = %d, want 200", rr.Code)
	}

	// Cookie (no query/header): the follow-up fetch after the QR landing.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/daily", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "good-token"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("cookie token = %d, want 200", rr.Code)
	}
}

func TestCache_ReusesWithinTTLThenExpires(t *testing.T) {
	var calls int32
	loader := func() (aggregate.Snapshot, error) {
		atomic.AddInt32(&calls, 1)
		return fixtureLoader()
	}
	// Mutable clock so we can advance past the TTL deterministically.
	var mu sync.Mutex
	now := fixedNow()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	h := New(loader, WithClock(clock), WithLocation(time.UTC), WithCacheTTL(10*time.Second)).Handler()

	hit := func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
	}

	hit() // cold: 1 load
	hit() // within TTL: cached
	hit() // within TTL: cached
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("loads within TTL = %d, want 1 (cached)", got)
	}

	advance(11 * time.Second) // past TTL
	hit()                     // reload
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("loads after TTL expiry = %d, want 2", got)
	}
}

func TestCache_SingleFlightCoalescesConcurrentLoads(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	loader := func() (aggregate.Snapshot, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the load open so all requests pile up on the same flight
		return fixtureLoader()
	}
	h := New(loader, WithClock(fixedNow), WithLocation(time.UTC), WithCacheTTL(10*time.Second)).Handler()

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
		}()
	}
	// Let the goroutines reach the loader / the single-flight wait, then release.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("concurrent loads = %d, want 1 (single-flight)", got)
	}
}

func TestCache_DisabledWhenTTLZero(t *testing.T) {
	var calls int32
	loader := func() (aggregate.Snapshot, error) {
		atomic.AddInt32(&calls, 1)
		return fixtureLoader()
	}
	h := New(loader, WithClock(fixedNow), WithLocation(time.UTC), WithCacheTTL(0)).Handler()
	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("loads with cache disabled = %d, want 3", got)
	}
}

func TestAdvice_DefaultsToAllHistoryNotToday(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/advice", nil)
	newTestServer().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var report advise.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Window != string(aggregate.WindowAll) {
		t.Errorf("window = %q, want %q: a trend needs history, so advice defaults to all",
			report.Window, aggregate.WindowAll)
	}
	// The fixture has 3 turns, one of them 3 days back — "today" would only see 2.
	if report.Fleet.Turns != 3 {
		t.Errorf("fleet turns = %d, want 3 (the whole fixture)", report.Fleet.Turns)
	}
	if report.CostLabel != aggregate.CostLabel {
		t.Errorf("cost label = %q, want the mandatory wording", report.CostLabel)
	}
}

func TestAdvice_HonorsExplicitWindow(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/advice?window=today", nil)
	newTestServer().ServeHTTP(rr, req)

	var report advise.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Window != string(aggregate.WindowToday) {
		t.Errorf("window = %q, want %q", report.Window, aggregate.WindowToday)
	}
	if report.Fleet.Turns != 2 {
		t.Errorf("fleet turns = %d, want 2 (today only)", report.Fleet.Turns)
	}
}

func TestAdvice_LoaderErrorIs500(t *testing.T) {
	failing := func() (aggregate.Snapshot, error) { return aggregate.Snapshot{}, errBoom }
	srv := New(failing, WithClock(fixedNow), WithLocation(time.UTC)).Handler()

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/advice", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

func TestQuota_ReturnsReportFromAnalyze(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/quota", nil)
	newTestServer().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var report quota.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.GeneratedAt.IsZero() {
		t.Error("generatedAt should be set")
	}
	// Fixture has no session-limit events: calibration stays unknown and the
	// report must still round-trip without inventing a ceiling.
	if report.Calibration.Capacity.Known {
		t.Error("fixture has no exhaustions: capacity must stay unknown, not invented")
	}
	// Breakdown covers the default 3-day window; the 3-days-ago turn is on the
	// boundary depending on clock, but today's two turns are always in.
	if report.Breakdown.Total.Turns < 2 {
		t.Errorf("breakdown turns = %d, want at least today's 2", report.Breakdown.Total.Turns)
	}
}

// contextSwitchLoader is the T22 case as a fixture: one measured session that
// changed model mid-conversation, carrying 600k of context into a window five
// times smaller.
func contextSwitchLoader() (aggregate.Snapshot, error) {
	now := fixedNow()
	return aggregate.Snapshot{Records: []aggregate.Record{
		{
			Agent: aggregate.AgentClaudeCode, Confidence: aggregate.ConfidenceMeasured,
			SessionID: "sess-switch", Workspace: "/home/user/Develop/llm-agent-spend-manager",
			Timestamp: now.Add(-2 * time.Hour), Model: "claude-sonnet-5", CacheRead: 600_000,
		},
		{
			Agent: aggregate.AgentClaudeCode, Confidence: aggregate.ConfidenceMeasured,
			SessionID: "sess-switch", Workspace: "/home/user/Develop/llm-agent-spend-manager",
			Timestamp: now.Add(-1 * time.Hour), Model: "claude-haiku-4-5", CacheRead: 600_000,
		},
	}}, nil
}

// The JSON API has to carry the same measurement the CLI prints, from the same
// code path — three surfaces that disagree are three sources of truth.
func TestQuota_CarriesTheContextWindowSection(t *testing.T) {
	rr := httptest.NewRecorder()
	handler := New(contextSwitchLoader, WithClock(fixedNow), WithLocation(time.UTC)).Handler()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/quota", nil))

	var report quota.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cw := report.ContextWindows
	if len(cw.Shifts) != 1 {
		t.Fatalf("shifts = %d, want 1", len(cw.Shifts))
	}
	if got, want := cw.Shifts[0].Shift.Before.Share, 0.60; got != want {
		t.Errorf("share antes = %v, want %v", got, want)
	}
	if got, want := cw.Shifts[0].Shift.After.Share, 3.0; got != want {
		t.Errorf("share después = %v, want %v", got, want)
	}
	if len(cw.Streams) != 1 || cw.Streams[0].Stream.Status != "ceiling" {
		t.Errorf("streams = %+v, want una sesión marcada arriba del techo", cw.Streams)
	}
	if cw.Blind[aggregate.AgentCursor] == "" {
		t.Error("los agentes sin contexto medible no viajan en el JSON")
	}
}

func TestQuota_HonorsTheContextWarningThresholdFromTheEnvironment(t *testing.T) {
	rr := httptest.NewRecorder()
	handler := New(contextSwitchLoader,
		WithClock(fixedNow), WithLocation(time.UTC),
		WithGetenv(func(key string) string {
			if key == quota.EnvContextWarnPct {
				return "55"
			}
			return ""
		}),
	).Handler()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/quota", nil))

	var report quota.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := report.ContextWindows.WarnAt, 0.55; got != want {
		t.Errorf("warnAt = %v, want %v", got, want)
	}
}

func TestQuota_HonorsDaysQuery(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/quota?days=0", nil)
	newTestServer().ServeHTTP(rr, req)

	var report quota.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Breakdown.Total.Turns != 3 {
		t.Errorf("days=0 turns = %d, want 3 (all history)", report.Breakdown.Total.Turns)
	}
	if !report.Breakdown.Since.IsZero() {
		t.Errorf("days=0 since = %v, want zero time (all history)", report.Breakdown.Since)
	}
}

func TestQuota_RejectsNegativeDays(t *testing.T) {
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/quota?days=-1", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestQuota_LoaderErrorIs500(t *testing.T) {
	failing := func() (aggregate.Snapshot, error) { return aggregate.Snapshot{}, errBoom }
	h := New(failing, WithClock(fixedNow), WithLocation(time.UTC)).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/quota", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

func TestOutcome_NotConfiguredIs501(t *testing.T) {
	// newTestServer has no ChangesLoader: the route must refuse rather than
	// invent an empty ledger that would read as "nothing happened".
	rr := httptest.NewRecorder()
	newTestServer().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/outcome", nil))
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rr.Code)
	}
}

func TestOutcome_ReturnsLedgerWithChangesAndAttribution(t *testing.T) {
	now := fixedNow()
	changes := outcome.ChangeLedger{
		Changes: []outcome.Change{{
			At:     now.Add(-2 * 24 * time.Hour),
			Source: outcome.SourceGit,
			Repo:   "llm-agent-spend-manager",
			Ref:    "abc1234",
			Actor:  "claude-code",
			Note:   "wire outcome API",
		}},
		Repos:   []string{"llm-agent-spend-manager"},
		Commits: 1,
	}
	h := New(fixtureLoader,
		WithClock(fixedNow),
		WithLocation(time.UTC),
		WithChangesLoader(func(context.Context) (outcome.ChangeLedger, error) {
			return changes, nil
		}),
	).Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/outcome", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp outcomeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != string(aggregate.WindowAll) {
		t.Errorf("window = %q, want all (level shifts need history)", resp.Window)
	}
	if resp.Changes.Commits != 1 {
		t.Errorf("commits = %d, want 1", resp.Changes.Commits)
	}
	if len(resp.Outcomes.Outcomes) == 0 {
		t.Error("outcomes should include tracked series")
	}
	if resp.CostLabel != aggregate.CostLabel {
		t.Errorf("costLabel = %q, want mandatory wording", resp.CostLabel)
	}
}

func TestOutcome_HonorsExplicitWindow(t *testing.T) {
	h := New(fixtureLoader,
		WithClock(fixedNow),
		WithLocation(time.UTC),
		WithChangesLoader(func(context.Context) (outcome.ChangeLedger, error) {
			return outcome.ChangeLedger{}, nil
		}),
	).Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/outcome?window=today", nil))
	var resp outcomeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != string(aggregate.WindowToday) {
		t.Errorf("window = %q, want today", resp.Window)
	}
}

func TestOutcome_ChangesLoaderErrorIs500(t *testing.T) {
	h := New(fixtureLoader,
		WithClock(fixedNow),
		WithLocation(time.UTC),
		WithChangesLoader(func(context.Context) (outcome.ChangeLedger, error) {
			return outcome.ChangeLedger{}, errBoom
		}),
	).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/outcome", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}
