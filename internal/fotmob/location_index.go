package fotmob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/0xjuanma/golazo/internal/data"
)

const (
	// LocationIndexFileName is the on-disk name of the shared match-location
	// index, stored in the golazo config directory alongside empty-results.json.
	LocationIndexFileName = "match-locations.json"

	locationIndexVersion = 1

	// MaxLocationEntries bounds the index so a long-running install cannot
	// grow the file without limit. Eviction drops the least recently updated
	// entries first.
	MaxLocationEntries = 500

	// locationEntryTTL bounds how long a stored page slug is trusted. A stale
	// slug is not fatal — MatchDetails relocates the match through its league
	// page when a fetch fails — but expiry keeps abandoned matches from being
	// retried forever.
	locationEntryTTL = 30 * 24 * time.Hour
)

// LocationEntry is the shared cross-process location contract: everything a
// later, independent process needs to fetch a specific match's details.
//
//   - MatchID identifies the match and is used to validate the response.
//   - LeagueID allows re-location when the stored PageURL no longer resolves.
//   - PageURL is the FotMob match page slug (e.g. "/matches/a-vs-b/abc123").
type LocationEntry struct {
	MatchID   int       `json:"match_id"`
	LeagueID  int       `json:"league_id"`
	PageURL   string    `json:"page_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

// locationFile is the JSON envelope persisted on disk.
type locationFile struct {
	Version int                      `json:"version"`
	Entries map[string]LocationEntry `json:"entries"`
}

// LocationIndex is a persistent, bounded, concurrency-safe index of match
// page locations. Writes are atomic (temp file + rename) so a reader in
// another process never observes a partially written file, and every Upsert
// re-reads the latest on-disk state before merging, which keeps concurrent
// processes from silently dropping each other's records.
//
// A corrupted file never breaks the caller: loads degrade to an empty index
// and the next successful Upsert repairs the file. Individual malformed
// records are discarded while valid records are kept.
type LocationIndex struct {
	mu       sync.RWMutex
	filePath string
	entries  map[string]LocationEntry
}

// indexPathLocks serializes Upserts targeting the same file path across
// distinct LocationIndex instances (e.g. separate clients in one process).
// Combined with atomic renames, this makes same-process concurrent writes
// loss-free and guarantees cross-process readers only ever see complete
// files.
var indexPathLocks sync.Map // string -> *sync.Mutex

func pathLockFor(filePath string) *sync.Mutex {
	m, _ := indexPathLocks.LoadOrStore(filePath, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// NewLocationIndex opens the index in the user's golazo config directory,
// creating the directory if needed. A missing file is a normal cold start;
// a corrupted file yields an empty index rather than an error.
func NewLocationIndex() (*LocationIndex, error) {
	configDir, err := data.ConfigDir()
	if err != nil {
		return nil, err
	}
	return newLocationIndexFromPath(filepath.Join(configDir, LocationIndexFileName)), nil
}

// newLocationIndexFromPath builds an index backed by an explicit file path.
// Used by tests and keeps the persistence layer decoupled from config-dir
// resolution. Never returns an error: disk problems surface on the next write
// while reads simply report no locations.
func newLocationIndexFromPath(filePath string) *LocationIndex {
	idx := &LocationIndex{
		filePath: filePath,
		entries:  make(map[string]LocationEntry),
	}
	idx.load()
	idx.pruneExpired()
	return idx
}

// Lookup returns the location recorded for matchID. Expired or unknown IDs
// report a miss; the stored record itself is left in place until the next
// write's prune pass so lookups stay allocation-free.
func (idx *LocationIndex) Lookup(matchID int) (LocationEntry, bool) {
	if idx == nil || matchID <= 0 {
		return LocationEntry{}, false
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	entry, ok := idx.entries[strconv.Itoa(matchID)]
	if !ok {
		return LocationEntry{}, false
	}
	if time.Since(entry.UpdatedAt) > locationEntryTTL {
		return LocationEntry{}, false
	}
	return entry, true
}

// Upsert merges entries into the index and persists atomically. Each entry is
// validated independently — one bad record cannot reject the batch. Before
// writing, the latest on-disk state is reloaded and merged so concurrent
// processes preserve each other's records. The index is pruned (expired and
// overflowing entries evicted) before every save.
func (idx *LocationIndex) Upsert(entries []LocationEntry) error {
	if idx == nil {
		return nil
	}

	// Serialize all in-process writers of this file, including separate
	// index instances, so reload-merge-write cycles cannot interleave.
	pathMu := pathLockFor(idx.filePath)
	pathMu.Lock()
	defer pathMu.Unlock()

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Re-read disk under the lock so a merge against stale in-memory state
	// cannot erase records another process wrote in the meantime.
	idx.load()

	now := time.Now()
	for _, entry := range entries {
		if !validLocationEntry(entry) {
			continue
		}
		if entry.UpdatedAt.IsZero() {
			entry.UpdatedAt = now
		}
		idx.entries[strconv.Itoa(entry.MatchID)] = entry
	}

	idx.pruneExpiredLocked()
	idx.enforceBoundLocked()

	return idx.saveLocked()
}

// Len reports the number of currently stored entries (including unexpired
// ones only). Primarily used by tests and diagnostics.
func (idx *LocationIndex) Len() int {
	if idx == nil {
		return 0
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	count := 0
	now := time.Now()
	for _, entry := range idx.entries {
		if now.Sub(entry.UpdatedAt) <= locationEntryTTL {
			count++
		}
	}
	return count
}

// load reads and decodes the index file. A missing file leaves the index
// empty; a corrupted file or malformed record is tolerated: the index resets
// and individual invalid records are dropped without affecting the others.
func (idx *LocationIndex) load() {
	raw, err := os.ReadFile(idx.filePath)
	if err != nil {
		// Missing file (cold start) or unreadable path: start empty. The
		// next Upsert repairs/creates the file.
		idx.entries = make(map[string]LocationEntry)
		return
	}

	var onDisk locationFile
	if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk.Entries == nil {
		// Whole-file corruption: no record can be trusted, but callers must
		// not fail — degrade to empty and let the next write repair the file.
		idx.entries = make(map[string]LocationEntry)
		return
	}

	// Per-record validation isolates a single bad entry from the rest.
	cleaned := make(map[string]LocationEntry, len(onDisk.Entries))
	for key, entry := range onDisk.Entries {
		if !validLocationEntry(entry) || key != strconv.Itoa(entry.MatchID) {
			continue
		}
		cleaned[key] = entry
	}
	idx.entries = cleaned
}

// saveLocked serializes the index and writes it atomically: marshal once,
// write a temp file in the same directory, then rename over the target so
// concurrent readers (including other processes) only ever see a complete
// JSON document.
func (idx *LocationIndex) saveLocked() error {
	if dir := filepath.Dir(idx.filePath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	payload := locationFile{
		Version: locationIndexVersion,
		Entries: idx.entries,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(idx.filePath), ".match-locations-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, idx.filePath)
}

func (idx *LocationIndex) pruneExpired() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.pruneExpiredLocked()
}

func (idx *LocationIndex) pruneExpiredLocked() {
	now := time.Now()
	for key, entry := range idx.entries {
		if now.Sub(entry.UpdatedAt) > locationEntryTTL {
			delete(idx.entries, key)
		}
	}
}

// enforceBoundLocked evicts the least recently updated entries until the
// index fits MaxLocationEntries. Ties break on MatchID so eviction is
// deterministic across machines.
func (idx *LocationIndex) enforceBoundLocked() {
	if len(idx.entries) <= MaxLocationEntries {
		return
	}

	all := make([]LocationEntry, 0, len(idx.entries))
	for _, entry := range idx.entries {
		all = append(all, entry)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].UpdatedAt.Equal(all[j].UpdatedAt) {
			return all[i].UpdatedAt.Before(all[j].UpdatedAt)
		}
		return all[i].MatchID < all[j].MatchID
	})

	drop := len(all) - MaxLocationEntries
	for i := 0; i < drop; i++ {
		delete(idx.entries, strconv.Itoa(all[i].MatchID))
	}
}

// validLocationEntry enforces the location contract: a positive match ID and
// a non-empty page slug. League 0 (unknown league) is accepted — the entry
// still supports direct page fetches, only league-based relocation is lost.
func validLocationEntry(entry LocationEntry) bool {
	return entry.MatchID > 0 && entry.PageURL != ""
}
