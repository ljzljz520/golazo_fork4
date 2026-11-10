package fotmob

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xjuanma/golazo/internal/api"
	"github.com/0xjuanma/golazo/internal/ratelimit"
)

// Fixture slugs served by the local page fixture server.
const (
	freshSlug     = "/matches/home-vs-away/fresh"
	staleSlug     = "/matches/home-vs-away/stale"
	wrongSlug     = "/matches/wrong-match/x"
	slowSlug      = "/matches/slow/slug"
	fixtureLeague = 47
	fixtureMatch  = 12345
)

// fixtureRouter is an http.RoundTripper that serves local FotMob page
// fixtures entirely in memory. Production code builds absolute
// https://www.fotmob.com URLs, so tests intercept at the transport layer —
// no network is ever touched. Every requested path is recorded for order
// assertions.
type fixtureRouter struct {
	mu    sync.Mutex
	paths []string
}

func (r *fixtureRouter) record(path string) {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.mu.Unlock()
}

func (r *fixtureRouter) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

func (r *fixtureRouter) saw(path string) bool {
	for _, p := range r.requests() {
		if p == path {
			return true
		}
	}
	return false
}

func (r *fixtureRouter) roundTrip(req *http.Request) (*http.Response, error) {
	r.record(req.URL.Path)

	switch req.URL.Path {
	case "/leagues/47":
		return htmlResponse(leagueFixtureHTML()), nil
	case freshSlug:
		return htmlResponse(matchFixtureHTML(fixtureMatch)), nil
	case staleSlug:
		return statusResponse(http.StatusNotFound), nil
	case wrongSlug:
		// 200 OK but the embedded page belongs to a different match: this is
		// the identity-mismatch case the client must refuse to cache.
		return htmlResponse(matchFixtureHTML(99999)), nil
	case slowSlug:
		select {
		case <-time.After(300 * time.Millisecond):
			return htmlResponse(matchFixtureHTML(fixtureMatch)), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	default:
		// Legacy /api/matchDetails fallback and anything unknown: 404.
		return statusResponse(http.StatusNotFound), nil
	}
}

// newRoutingClient builds a Client wired to the fixture transport and a
// persistent location index under indexPath. Each call gets a fresh
// in-process pageURLs map, mirroring a brand-new OS process.
func newRoutingClient(t *testing.T, indexPath string, router *fixtureRouter) *Client {
	t.Helper()
	return &Client{
		httpClient:    &http.Client{Transport: roundTripperFunc(router.roundTrip), Timeout: 5 * time.Second},
		baseURL:       baseURL,
		rateLimiter:   ratelimit.New(0),
		cache:         NewResponseCache(DefaultCacheConfig()),
		pageURLs:      make(map[int]LocationEntry, 10),
		locationIndex: newLocationIndexFromPath(indexPath),
		maxConcurrent: make(chan struct{}, 10),
	}
}

func htmlResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/html"}},
	}
}

func statusResponse(code int) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
}

// nextDataHTML wraps pageProps in the Next.js __NEXT_DATA__ script tag the
// production extractor parses.
func nextDataHTML(pageProps any) string {
	raw, err := json.Marshal(pageProps)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(
		`<html><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":%s}}</script></html>`,
		string(raw),
	)
}

// leagueFixtureHTML is a league page containing one live match whose current
// page slug is freshSlug. Both the live classifier and the relocation parser
// consume this shape.
func leagueFixtureHTML() string {
	pageProps := map[string]any{
		"details": map[string]any{
			"id": fixtureLeague, "name": "Premier League", "country": "England",
		},
		"fixtures": map[string]any{
			"allMatches": []map[string]any{{
				"id":      strconv.Itoa(fixtureMatch),
				"pageUrl": freshSlug + "#" + strconv.Itoa(fixtureMatch),
				"home":    map[string]any{"id": "1", "name": "Home FC", "shortName": "HOM"},
				"away":    map[string]any{"id": "2", "name": "Away FC", "shortName": "AWY"},
				"league":  map[string]any{"id": fixtureLeague, "name": "Premier League", "country": "England"},
				"status": map[string]any{
					"utcTime":  "2026-11-10T19:00:00.000Z",
					"started":  true,
					"finished": false,
				},
			}},
		},
	}
	return nextDataHTML(pageProps)
}

// matchFixtureHTML is a match details page embedding the given match ID.
func matchFixtureHTML(matchID int) string {
	finished := true
	details := fotmobMatchDetails{}
	details.General.MatchID = strconv.Itoa(matchID)
	details.General.HomeTeam.ID = 1
	details.General.HomeTeam.Name = "Home FC"
	details.General.AwayTeam.ID = 2
	details.General.AwayTeam.Name = "Away FC"
	details.General.LeagueID = fixtureLeague
	details.General.LeagueName = "Premier League"
	details.Header.Status.UTCTime = "2026-11-10T19:00:00Z"
	details.Header.Status.Finished = &finished
	details.Header.Teams = []struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Score int    `json:"score"`
	}{
		{ID: 1, Name: "Home FC", Score: 2},
		{ID: 2, Name: "Away FC", Score: 1},
	}
	return nextDataHTML(details)
}

// TestMatchDetails_TwoProcessChaining is the core end-to-end contract: a list
// command in process A persists discovered locations, and a fresh process B
// resolves match details through the on-disk index alone.
func TestMatchDetails_TwoProcessChaining(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), LocationIndexFileName)
	router := &fixtureRouter{}

	// Process A: discover the live match through its league page.
	processA := newRoutingClient(t, indexPath, router)
	live, _, err := processA.LiveAndUpcomingForLeague(context.Background(), fixtureLeague)
	if err != nil {
		t.Fatalf("LiveAndUpcomingForLeague: %v", err)
	}
	if len(live) != 1 || live[0].ID != fixtureMatch {
		t.Fatalf("live matches = %+v, want single match %d", live, fixtureMatch)
	}

	// The location must be on disk for a process that never held it in memory.
	onDisk := newLocationIndexFromPath(indexPath)
	entry, ok := onDisk.Lookup(fixtureMatch)
	if !ok {
		t.Fatal("list command did not persist the match location")
	}
	if entry.LeagueID != fixtureLeague || entry.PageURL != freshSlug {
		t.Errorf("persisted entry = %+v, want league %d slug %q", entry, fixtureLeague, freshSlug)
	}

	// Process B: cold client, empty in-process map, same index file.
	processB := newRoutingClient(t, indexPath, router)
	if got := processB.getPageURL(fixtureMatch); got != "" {
		t.Fatalf("fresh process must not hold in-memory slugs, got %q", got)
	}

	details, err := processB.MatchDetails(context.Background(), fixtureMatch)
	if err != nil {
		t.Fatalf("cold-start MatchDetails: %v", err)
	}
	if details == nil || details.ID != fixtureMatch {
		t.Fatalf("details = %+v, want ID %d", details, fixtureMatch)
	}
	if !router.saw(freshSlug) {
		t.Errorf("expected match page request to %q, requests=%v", freshSlug, router.requests())
	}
	if processB.cache.Details(fixtureMatch) == nil {
		t.Error("validated details should be cached after success")
	}
}

// TestMatchDetails_RelocatesWhenStoredSlugInvalid covers failure recovery:
// the persisted slug 404s, so the client re-fetches the known league page,
// discovers the current slug, repairs the index, and returns details.
func TestMatchDetails_RelocatesWhenStoredSlugInvalid(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), LocationIndexFileName)
	router := &fixtureRouter{}

	// Simulate a stale index written by an earlier process.
	seed := newLocationIndexFromPath(indexPath)
	if err := seed.Upsert([]LocationEntry{{
		MatchID:  fixtureMatch,
		LeagueID: fixtureLeague,
		PageURL:  staleSlug,
	}}); err != nil {
		t.Fatalf("seed stale location: %v", err)
	}

	client := newRoutingClient(t, indexPath, router)
	details, err := client.MatchDetails(context.Background(), fixtureMatch)
	if err != nil {
		t.Fatalf("MatchDetails with stale slug: %v", err)
	}
	if details == nil || details.ID != fixtureMatch {
		t.Fatalf("details = %+v, want ID %d after relocation", details, fixtureMatch)
	}

	requests := router.requests()
	staleAt, leagueAt, freshAt := -1, -1, -1
	for i, p := range requests {
		switch p {
		case staleSlug:
			staleAt = i
		case "/leagues/47":
			leagueAt = i
		case freshSlug:
			freshAt = i
		}
	}
	if staleAt == -1 || leagueAt == -1 || freshAt == -1 {
		t.Fatalf("expected stale->league->fresh request sequence, got %v", requests)
	}
	if !(staleAt < leagueAt && leagueAt < freshAt) {
		t.Errorf("wrong request order: stale=%d league=%d fresh=%d (%v)", staleAt, leagueAt, freshAt, requests)
	}

	// The index must be repaired for subsequent processes.
	repaired := newLocationIndexFromPath(indexPath)
	entry, ok := repaired.Lookup(fixtureMatch)
	if !ok || entry.PageURL != freshSlug {
		t.Errorf("index not repaired: entry=%+v ok=%v, want slug %q", entry, ok, freshSlug)
	}
}

// TestMatchDetails_ErrorAndMismatchPagesAreNeverCached ensures neither an HTTP
// error page nor a 200 page belonging to another match can enter the details
// cache or be returned to the caller.
func TestMatchDetails_ErrorAndMismatchPagesAreNeverCached(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), LocationIndexFileName)
	router := &fixtureRouter{}

	// Case 1: identity mismatch (league unknown, so no relocation is possible).
	seed := newLocationIndexFromPath(indexPath)
	if err := seed.Upsert([]LocationEntry{{
		MatchID: fixtureMatch, LeagueID: 0, PageURL: wrongSlug,
	}}); err != nil {
		t.Fatalf("seed wrong-match location: %v", err)
	}

	client := newRoutingClient(t, indexPath, router)
	details, err := client.MatchDetails(context.Background(), fixtureMatch)
	if err == nil {
		t.Fatalf("mismatched page must not be accepted, got details=%+v", details)
	}
	if client.cache.Details(fixtureMatch) != nil {
		t.Error("identity-mismatch page was written to the details cache")
	}
	if !router.saw(wrongSlug) {
		t.Errorf("wrong-match slug was never requested: %v", router.requests())
	}

	// Case 2: hard 404 on the stored slug, also unrecoverable (league 0).
	indexPath2 := filepath.Join(t.TempDir(), LocationIndexFileName)
	seed2 := newLocationIndexFromPath(indexPath2)
	if err := seed2.Upsert([]LocationEntry{{
		MatchID: fixtureMatch, LeagueID: 0, PageURL: staleSlug,
	}}); err != nil {
		t.Fatalf("seed 404 location: %v", err)
	}
	router2 := &fixtureRouter{}
	client2 := newRoutingClient(t, indexPath2, router2)
	if _, err := client2.MatchDetails(context.Background(), fixtureMatch); err == nil {
		t.Fatal("404 page must surface an error, not a cached empty result")
	}
	if client2.cache.Details(fixtureMatch) != nil {
		t.Error("404 error page was written to the details cache")
	}
}

// TestMatchDetails_TimeoutFailsOpenAndDoesNotCache verifies the deadline
// behavior end to end: a slow page plus a short context returns an error,
// leaves the cache empty, and never produces a phantom success.
func TestMatchDetails_TimeoutFailsOpenAndDoesNotCache(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), LocationIndexFileName)
	seed := newLocationIndexFromPath(indexPath)
	if err := seed.Upsert([]LocationEntry{{
		MatchID: fixtureMatch, LeagueID: 0, PageURL: slowSlug,
	}}); err != nil {
		t.Fatalf("seed slow location: %v", err)
	}

	router := &fixtureRouter{}
	client := newRoutingClient(t, indexPath, router)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.MatchDetails(ctx, fixtureMatch)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("call took %s, should return promptly on deadline", elapsed)
	}
	if client.cache.Details(fixtureMatch) != nil {
		t.Error("timed-out fetch must not populate the details cache")
	}
}

// TestMatchDetails_UnknownIDFallsBackToAPI documents the preserved legacy
// contract: with no location known anywhere, MatchDetails still tries the
// direct API endpoint rather than inventing a page URL.
func TestMatchDetails_UnknownIDFallsBackToAPI(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), LocationIndexFileName)
	router := &fixtureRouter{}
	client := newRoutingClient(t, indexPath, router)

	if _, err := client.MatchDetails(context.Background(), 77777); err == nil {
		t.Fatal("unknown ID should fail against the 404 fixture, not succeed")
	}
	foundAPIFallback := false
	for _, p := range router.requests() {
		if strings.HasPrefix(p, "/api/matchDetails") {
			foundAPIFallback = true
		}
	}
	if !foundAPIFallback {
		t.Errorf("expected legacy /api/matchDetails fallback, requests=%v", router.requests())
	}
	if client.cache.Details(77777) != nil {
		t.Error("failed fallback must not write the cache")
	}
}

// TestPersistMatchLocations_BestEffortNilIndex guards clients constructed
// without a persistent index (e.g. struct-literal test clients): persistence
// must be a no-op rather than a panic.
func TestPersistMatchLocations_BestEffortNilIndex(t *testing.T) {
	client := newTestClient("http://unused")
	client.PersistMatchLocations([]api.Match{
		{ID: 1, League: api.League{ID: 47}, PageURL: "/matches/a/b"},
		{ID: 2, PageURL: ""}, // skipped: no slug
	})
	if got := client.getPageURL(1); got != "/matches/a/b" {
		t.Errorf("in-memory slug = %q, want /matches/a/b", got)
	}
	if got := client.getPageURL(2); got != "" {
		t.Errorf("slugless match must not be stored, got %q", got)
	}
}
