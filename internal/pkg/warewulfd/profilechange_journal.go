package warewulfd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// Profile change journal phases. A record advances strictly in this
// order; a failure leaves the record in its current phase so that a later
// recovery run can resume from the same point. A record whose every build
// item is done is pruned from the journal rather than persisted in a
// terminal phase.
const (
	phasePublish  = "publish"
	phaseAdopt    = "adopt"
	phaseOverlays = "overlays"
)

// Per-context overlay build states.
const (
	stPending = "pending"
	stDone    = "done"
	stFailed  = "failed"
)

// journalVersion is the on-disk format version of the journal document.
const journalVersion = 1

// overlayBuildState tracks the build of one overlay context (system or
// runtime) for one node.
type overlayBuildState struct {
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
}

// affectedNodeState groups the two independent overlay builds of a node.
type affectedNodeState struct {
	Node    string            `json:"node"`
	System  overlayBuildState `json:"system"`
	Runtime overlayBuildState `json:"runtime"`
}

// profileChangeRecord is the write-ahead record of one profile update
// transaction. ConfigYAML holds the full nodes.conf content to publish,
// allowing the publish phase to resume even if the originating process
// died before the atomic rename.
type profileChangeRecord struct {
	ID         string              `json:"id"`
	Roots      []string            `json:"roots"`
	StartedAt  time.Time           `json:"started_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Phase      string              `json:"phase"`
	PhaseError string              `json:"phase_error,omitempty"`
	ConfigYAML string              `json:"config_yaml"`
	ConfigHash string              `json:"config_hash"`
	AutoBuild  bool                `json:"autobuild"`
	Nodes      []affectedNodeState `json:"nodes,omitempty"`
}

// profileChangeJournal is the on-disk JSON document.
type profileChangeJournal struct {
	Version int                    `json:"version"`
	Changes []*profileChangeRecord `json:"changes"`
}

// profileChangeStore serializes access to the journal file from one
// process. Because a separate wwctl process may publish the journal via
// temp-file + rename and signal this daemon through SIGHUP, the cached
// document is validated against the file's identity (existence, mtime,
// size) on every access and re-read when an external writer changed it.
type profileChangeStore struct {
	mu       sync.Mutex
	path     string
	journal  *profileChangeJournal
	loaded   bool
	present  bool
	statMod  time.Time
	statSize int64
}

var (
	profileChanges = &profileChangeStore{}

	// journalSeq disambiguates records sharing the same nanosecond stamp.
	journalSeq atomic.Uint64

	// writeJournalFile is indirected so tests can inject an I/O failure.
	writeJournalFile = util.AtomicWriteFile
)

// profileChangeJournalPath resolves the journal location under the
// configured provision directory.
func profileChangeJournalPath() string {
	return path.Join(warewulfconf.Get().Paths.WWProvisiondir, "profile-change.json")
}

// resetProfileChangeStore discards the cached journal so the next access
// unconditionally re-reads it from disk. Test-only: it simulates a fresh
// process.
func resetProfileChangeStore() {
	profileChanges.mu.Lock()
	defer profileChanges.mu.Unlock()
	profileChanges.journal = nil
	profileChanges.path = ""
	profileChanges.loaded = false
	profileChanges.present = false
}

// identityUnchanged reports whether the journal file still matches the
// document cached in memory.
func (s *profileChangeStore) identityUnchanged(journalPath string, info os.FileInfo) bool {
	return s.loaded && s.path == journalPath && s.present &&
		info.ModTime().Equal(s.statMod) && info.Size() == s.statSize
}

// cacheIdentity records the file identity the cached journal was read
// from or most recently written as.
func (s *profileChangeStore) cacheIdentity(journalPath string, info os.FileInfo) {
	s.path = journalPath
	s.loaded = true
	s.present = true
	s.statMod = info.ModTime()
	s.statSize = info.Size()
}

// loadLocked re-reads and parses the journal whenever the file identity
// changed (external rename publication) or the configured path changed.
// A missing file is an empty journal; malformed content is returned as an
// error so callers can log it without silently discarding recovery state.
func (s *profileChangeStore) loadLocked() error {
	journalPath := profileChangeJournalPath()
	info, statErr := os.Stat(journalPath)
	if statErr == nil && s.identityUnchanged(journalPath, info) {
		return nil
	}

	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("could not stat profile change journal: %w", statErr)
	}

	if statErr != nil {
		// file absent (possibly removed by an external writer)
		s.journal = &profileChangeJournal{Version: journalVersion}
		s.path = journalPath
		s.loaded = true
		s.present = false
		s.statMod = time.Time{}
		s.statSize = 0
		return nil
	}

	data, err := os.ReadFile(journalPath)
	if err != nil {
		return fmt.Errorf("could not read profile change journal: %w", err)
	}
	parsed := &profileChangeJournal{}
	if err := json.Unmarshal(data, parsed); err != nil {
		return fmt.Errorf("could not parse profile change journal: %w", err)
	}
	if parsed.Version == 0 {
		parsed.Version = journalVersion
	}
	s.journal = parsed
	s.cacheIdentity(journalPath, info)
	return nil
}

// saveLocked atomically writes the journal. With no pending records the
// file is removed, leaving no trace of completed transactions. The cached
// file identity is refreshed afterwards; if an external writer changed
// the file concurrently (identity observed to differ), the cache is
// invalidated so the next access re-reads the merged-on-disk state.
func (s *profileChangeStore) saveLocked() error {
	journalPath := profileChangeJournalPath()
	if len(s.journal.Changes) == 0 {
		if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.path = journalPath
		s.present = false
		s.statMod = time.Time{}
		s.statSize = 0
		return nil
	}
	if err := os.MkdirAll(path.Dir(journalPath), 0o755); err != nil {
		return fmt.Errorf("could not create journal directory: %w", err)
	}
	data, err := json.MarshalIndent(s.journal, "", "  ")
	if err != nil {
		return err
	}
	if err := writeJournalFile(journalPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("could not persist profile change journal: %w", err)
	}
	if info, err := os.Stat(journalPath); err == nil {
		s.cacheIdentity(journalPath, info)
	} else {
		s.loaded = false
	}
	return nil
}

// listProfileChanges returns a copy of the pending change records.
func (s *profileChangeStore) list() ([]*profileChangeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	out := make([]*profileChangeRecord, len(s.journal.Changes))
	copy(out, s.journal.Changes)
	return out, nil
}

// addProfileChange appends a new record and persists the journal.
func (s *profileChangeStore) add(rec *profileChangeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.journal.Changes = append(s.journal.Changes, rec)
	if err := s.saveLocked(); err != nil {
		// keep in-memory state consistent with the failed write
		s.journal.Changes = s.journal.Changes[:len(s.journal.Changes)-1]
		return err
	}
	wwlog.Debug("profile change journal: added record %s (phase: %s, roots: %v)", rec.ID, rec.Phase, rec.Roots)
	return nil
}

// updateProfileChange stores the mutated record and persists the journal.
func (s *profileChangeStore) update(rec *profileChangeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	for i, existing := range s.journal.Changes {
		if existing.ID == rec.ID {
			rec.UpdatedAt = time.Now()
			s.journal.Changes[i] = rec
			return s.saveLocked()
		}
	}
	return fmt.Errorf("profile change record not found: %s", rec.ID)
}

// removeProfileChange deletes a record by ID.
func (s *profileChangeStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	kept := s.journal.Changes[:0]
	found := false
	for _, rec := range s.journal.Changes {
		if rec.ID == id {
			found = true
			continue
		}
		kept = append(kept, rec)
	}
	if !found {
		return fmt.Errorf("profile change record not found: %s", id)
	}
	s.journal.Changes = kept
	return s.saveLocked()
}

// newProfileChangeID returns a process-unique record identifier.
func newProfileChangeID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), journalSeq.Add(1))
}
