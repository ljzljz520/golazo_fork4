package fotmob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func indexPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), LocationIndexFileName)
}

// TestLocationIndex_TwoProcessChaining models two independent processes:
// process A upserts locations, process B opens a fresh index over the same
// file and must recover them on cold start with no in-memory state.
func TestLocationIndex_TwoProcessChaining(t *testing.T) {
	path := indexPath(t)

	processA := newLocationIndexFromPath(path)
	if err := processA.Upsert([]LocationEntry{{
		MatchID:  4506424,
		LeagueID: 47,
		PageURL:  "/matches/chelsea-vs-tottenham/abc123",
	}}); err != nil {
		t.Fatalf("process A upsert: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index file not persisted: %v", err)
	}

	// A brand-new instance simulates a cold-started second process.
	processB := newLocationIndexFromPath(path)
	entry, ok := processB.Lookup(4506424)
	if !ok {
		t.Fatal("process B cold-start lookup missed record written by process A")
	}
	if entry.LeagueID != 47 || entry.PageURL != "/matches/chelsea-vs-tottenham/abc123" {
		t.Errorf("process B entry = %+v, want league 47 and persisted slug", entry)
	}
}

// TestLocationIndex_CrossProcessMerge ensures one process's write does not
// erase another process's record: each Upsert reloads disk before merging.
func TestLocationIndex_CrossProcessMerge(t *testing.T) {
	path := indexPath(t)

	a := newLocationIndexFromPath(path)
	if err := a.Upsert([]LocationEntry{{MatchID: 1, LeagueID: 47, PageURL: "/matches/a/one"}}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}

	b := newLocationIndexFromPath(path)
	if err := b.Upsert([]LocationEntry{{MatchID: 2, LeagueID: 77, PageURL: "/matches/b/two"}}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	c := newLocationIndexFromPath(path)
	for _, id := range []int{1, 2} {
		if _, ok := c.Lookup(id); !ok {
			t.Errorf("record %d lost across interleaved process writes", id)
		}
	}
}

// TestLocationIndex_CorruptFileTolerated: a fully corrupted index must not
// fail construction; the next successful write repairs the file, and the
// repaired file is readable by another process.
func TestLocationIndex_CorruptFileTolerated(t *testing.T) {
	path := indexPath(t)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	idx := newLocationIndexFromPath(path)
	if _, ok := idx.Lookup(123); ok {
		t.Fatal("corrupt file must not yield records")
	}

	if err := idx.Upsert([]LocationEntry{{MatchID: 123, LeagueID: 47, PageURL: "/matches/repaired/slug"}}); err != nil {
		t.Fatalf("repair upsert: %v", err)
	}

	// The on-disk file must now be valid JSON carrying the repaired record.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repaired file: %v", err)
	}
	var parsed locationFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("repaired file is not valid JSON: %v", err)
	}
	if _, ok := parsed.Entries["123"]; !ok {
		t.Error("repaired file missing record 123")
	}

	fresh := newLocationIndexFromPath(path)
	if _, ok := fresh.Lookup(123); !ok {
		t.Error("repaired record not visible to a cold-started process")
	}
}

// TestLocationIndex_PerRecordCorruptionIsolated: one malformed record inside
// an otherwise valid file must be dropped while the valid records survive.
func TestLocationIndex_PerRecordCorruptionIsolated(t *testing.T) {
	path := indexPath(t)

	good := LocationEntry{MatchID: 42, LeagueID: 47, PageURL: "/matches/good/slug", UpdatedAt: time.Now()}
	onDisk := locationFile{
		Version: locationIndexVersion,
		Entries: map[string]LocationEntry{
			"42": good,
			// Key/ID mismatch — cannot be trusted.
			"7": {MatchID: 99, LeagueID: 47, PageURL: "/matches/key-mismatch/slug", UpdatedAt: time.Now()},
			// Empty page URL violates the location contract.
			"99": {MatchID: 99, LeagueID: 47, PageURL: "", UpdatedAt: time.Now()},
		},
	}
	raw, err := json.Marshal(onDisk)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	idx := newLocationIndexFromPath(path)
	entry, ok := idx.Lookup(42)
	if !ok {
		t.Fatal("valid record 42 should survive malformed neighbors")
	}
	if entry.PageURL != "/matches/good/slug" {
		t.Errorf("record 42 = %+v", entry)
	}
	if _, ok := idx.Lookup(99); ok {
		t.Error("empty-slug record must be rejected on load")
	}
	if idx.Len() != 1 {
		t.Errorf("Len = %d, want 1 (only the valid record)", idx.Len())
	}

	// A subsequent write must not resurrect the bad records.
	if err := idx.Upsert([]LocationEntry{{MatchID: 43, LeagueID: 47, PageURL: "/matches/another/slug"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	again := newLocationIndexFromPath(path)
	if _, ok := again.Lookup(99); ok {
		t.Error("invalid record resurrected after rewrite")
	}
	if _, ok := again.Lookup(42); !ok {
		t.Error("valid record lost during rewrite")
	}
}

// TestLocationIndex_BoundedEviction verifies the hard size cap and that the
// least recently updated entries are evicted first.
func TestLocationIndex_BoundedEviction(t *testing.T) {
	path := indexPath(t)
	idx := newLocationIndexFromPath(path)

	base := time.Now()
	entries := make([]LocationEntry, 0, MaxLocationEntries+10)
	for i := 1; i <= MaxLocationEntries+10; i++ {
		entries = append(entries, LocationEntry{
			MatchID:   i,
			LeagueID:  47,
			PageURL:   "/matches/m/" + strconv.Itoa(i),
			UpdatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	if err := idx.Upsert(entries); err != nil {
		t.Fatalf("bulk upsert: %v", err)
	}

	if got := idx.Len(); got != MaxLocationEntries {
		t.Errorf("Len = %d, want capped at %d", got, MaxLocationEntries)
	}

	// The ten oldest (IDs 1..10) must be evicted; the newest kept.
	for _, oldID := range []int{1, 5, 10} {
		if _, ok := idx.Lookup(oldID); ok {
			t.Errorf("oldest entry %d should have been evicted", oldID)
		}
	}
	newest := MaxLocationEntries + 10
	if _, ok := idx.Lookup(newest); !ok {
		t.Errorf("newest entry %d should be retained", newest)
	}

	// The cap must also hold on disk for cold-started processes.
	fresh := newLocationIndexFromPath(path)
	if got := fresh.Len(); got != MaxLocationEntries {
		t.Errorf("on-disk Len = %d, want %d", got, MaxLocationEntries)
	}
}

// TestLocationIndex_ExpiredEntriesMiss verifies TTL lazy-expiry on lookup.
func TestLocationIndex_ExpiredEntriesMiss(t *testing.T) {
	path := indexPath(t)
	idx := newLocationIndexFromPath(path)

	if err := idx.Upsert([]LocationEntry{{
		MatchID:   7,
		LeagueID:  47,
		PageURL:   "/matches/old/slug",
		UpdatedAt: time.Now().Add(-locationEntryTTL - time.Hour),
	}}); err != nil {
		t.Fatalf("upsert stale entry: %v", err)
	}

	if _, ok := idx.Lookup(7); ok {
		t.Error("expired entry must report a lookup miss")
	}

	// A fresh write prunes the expired record from disk.
	if err := idx.Upsert([]LocationEntry{{MatchID: 8, LeagueID: 47, PageURL: "/matches/fresh/slug"}}); err != nil {
		t.Fatalf("upsert fresh entry: %v", err)
	}
	fresh := newLocationIndexFromPath(path)
	if _, ok := fresh.Lookup(7); ok {
		t.Error("expired entry survived prune on disk")
	}
	if _, ok := fresh.Lookup(8); !ok {
		t.Error("fresh entry missing after prune")
	}
}

// TestLocationIndex_ConcurrentUpsert hammers the index from many goroutines
// with distinct match IDs. Every record must survive, the file must remain
// valid JSON after each write, and the bound must be respected.
func TestLocationIndex_ConcurrentUpsert(t *testing.T) {
	path := indexPath(t)

	const goroutines = 16
	const perGoroutine = 10

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			idx := newLocationIndexFromPath(path)
			for i := 0; i < perGoroutine; i++ {
				id := g*perGoroutine + i + 1
				if err := idx.Upsert([]LocationEntry{{
					MatchID:  id,
					LeagueID: 47,
					PageURL:  "/matches/concurrent/" + strconv.Itoa(id),
				}}); err != nil {
					t.Errorf("goroutine %d upsert %d: %v", g, id, err)
					return
				}
				// The file must always be parseable — atomic writes must never
				// expose a half-written document to a concurrent reader.
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("read during concurrent write: %v", err)
					return
				}
				var probe locationFile
				if err := json.Unmarshal(raw, &probe); err != nil {
					t.Errorf("index file invalid during concurrent writes: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	final := newLocationIndexFromPath(path)
	want := goroutines * perGoroutine
	if got := final.Len(); got != want {
		t.Errorf("Len = %d, want %d (concurrent records must not be lost)", got, want)
	}
	for id := 1; id <= want; id++ {
		if _, ok := final.Lookup(id); !ok {
			t.Errorf("record %d lost under concurrent upserts", id)
		}
	}

	// No temp files may be left behind by the atomic-write path.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// TestLocationIndex_InvalidEntriesRejected checks the contract validation at
// the Upsert boundary so bad data can never enter the shared index.
func TestLocationIndex_InvalidEntriesRejected(t *testing.T) {
	path := indexPath(t)
	idx := newLocationIndexFromPath(path)

	err := idx.Upsert([]LocationEntry{
		{MatchID: 0, LeagueID: 47, PageURL: "/matches/no-id"},     // missing ID
		{MatchID: -1, LeagueID: 47, PageURL: "/matches/neg-id"},   // negative ID
		{MatchID: 5, LeagueID: 47, PageURL: ""},                   // missing slug
		{MatchID: 6, LeagueID: 47, PageURL: "/matches/valid/one"}, // valid
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if idx.Len() != 1 {
		t.Errorf("Len = %d, want 1 (only the valid entry)", idx.Len())
	}
	if _, ok := idx.Lookup(6); !ok {
		t.Error("valid entry missing alongside rejected invalid entries")
	}
}
