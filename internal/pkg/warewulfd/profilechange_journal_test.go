package warewulfd

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/warewulf/warewulf/internal/pkg/testenv"
	"github.com/warewulf/warewulf/internal/pkg/util"
)

func sampleRecord(id string) *profileChangeRecord {
	now := time.Now()
	return &profileChangeRecord{
		ID:         id,
		Roots:      []string{"base"},
		StartedAt:  now,
		UpdatedAt:  now,
		Phase:      phasePublish,
		ConfigYAML: "nodeprofiles:\n  base: {}\nnodes: {}\n",
		ConfigHash: "abc",
		AutoBuild:  true,
		Nodes: []affectedNodeState{
			{
				Node:    "n1",
				System:  overlayBuildState{Status: stPending, UpdatedAt: now},
				Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
			},
		},
	}
}

func TestProfileChangeJournalLifecycle(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()
	resetProfileChangeStore()
	journalFile := profileChangeJournalPath()

	// missing file reads as an empty journal
	listed, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, listed)

	rec := sampleRecord("rec-1")
	assert.NoError(t, profileChanges.add(rec))
	assert.FileExists(t, journalFile)

	info, err := os.Stat(journalFile)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// a fresh store re-reads the same state from disk
	resetProfileChangeStore()
	listed, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Len(t, listed, 1)
	assert.Equal(t, "rec-1", listed[0].ID)
	assert.Equal(t, phasePublish, listed[0].Phase)
	assert.Equal(t, []string{"base"}, listed[0].Roots)
	assert.Len(t, listed[0].Nodes, 1)
	assert.Equal(t, "n1", listed[0].Nodes[0].Node)
	assert.Equal(t, stPending, listed[0].Nodes[0].System.Status)

	// update persists changes
	rec, err = func() (*profileChangeRecord, error) {
		records, lerr := profileChanges.list()
		return records[0], lerr
	}()
	assert.NoError(t, err)
	rec.Phase = phaseAdopt
	rec.PhaseError = ""
	assert.NoError(t, profileChanges.update(rec))
	resetProfileChangeStore()
	listed, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Equal(t, phaseAdopt, listed[0].Phase)
	assert.True(t, listed[0].UpdatedAt.After(listed[0].StartedAt) || listed[0].UpdatedAt.Equal(listed[0].StartedAt))

	// removing the last record removes the file itself
	assert.NoError(t, profileChanges.remove("rec-1"))
	_, err = os.Stat(journalFile)
	assert.True(t, os.IsNotExist(err))

	// removing an unknown record is an error
	assert.Error(t, profileChanges.remove("nope"))
}

func TestProfileChangeJournalWriteFailureKeepsPreviousState(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()
	resetProfileChangeStore()
	journalFile := profileChangeJournalPath()

	assert.NoError(t, profileChanges.add(sampleRecord("rec-1")))
	firstData, err := os.ReadFile(journalFile)
	assert.NoError(t, err)

	origin := writeJournalFile
	writeJournalFile = func(string, []byte, os.FileMode) error {
		return errors.New("injected journal write failure")
	}
	t.Cleanup(func() { writeJournalFile = origin })

	// failing add does not mutate the persisted state
	err = profileChanges.add(sampleRecord("rec-2"))
	assert.Error(t, err)
	resetProfileChangeStore()
	listed, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Len(t, listed, 1)
	assert.Equal(t, "rec-1", listed[0].ID)

	// failing update leaves the old bytes intact
	rec := listed[0]
	rec.Phase = phaseOverlays
	err = profileChanges.update(rec)
	assert.Error(t, err)
	persisted, err := os.ReadFile(journalFile)
	assert.NoError(t, err)
	assert.Equal(t, firstData, persisted)

	// after the fault clears, writes succeed again
	writeJournalFile = origin
	assert.NoError(t, profileChanges.update(rec))
	resetProfileChangeStore()
	listed, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Equal(t, phaseOverlays, listed[0].Phase)
}

func TestProfileChangeJournalCorruptFile(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()
	resetProfileChangeStore()

	assert.NoError(t, os.MkdirAll(path.Dir(profileChangeJournalPath()), 0o755))
	assert.NoError(t, util.AtomicWriteFile(profileChangeJournalPath(), []byte("{not json"), 0o600))

	_, err := profileChanges.list()
	assert.Error(t, err)
}

// A separate process (wwctl) publishes the journal via temp-file +
// rename while this daemon holds a cached (empty or populated) journal.
// The next store access must observe the external state, and a local add
// must merge with it instead of overwriting it.
func TestProfileChangeJournalExternalWriter(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()
	resetProfileChangeStore()
	journalFile := profileChangeJournalPath()
	assert.NoError(t, os.MkdirAll(path.Dir(journalFile), 0o755))

	// warm the cache with an absent journal
	listed, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, listed)

	// external writer (e.g. wwctl) publishes a record
	foreign := &profileChangeJournal{Version: journalVersion, Changes: []*profileChangeRecord{sampleRecord("foreign-1")}}
	data, err := json.MarshalIndent(foreign, "", "  ")
	assert.NoError(t, err)
	assert.NoError(t, util.AtomicWriteFile(journalFile, append(data, '\n'), 0o600))

	listed, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Len(t, listed, 1)
	assert.Equal(t, "foreign-1", listed[0].ID)

	// a local add merges with, and never clobbers, the external record
	assert.NoError(t, profileChanges.add(sampleRecord("daemon-1")))
	onDisk, err := os.ReadFile(journalFile)
	assert.NoError(t, err)
	assert.Contains(t, string(onDisk), "foreign-1")
	assert.Contains(t, string(onDisk), "daemon-1")

	// external removal is observed as well
	assert.NoError(t, os.Remove(journalFile))
	listed, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, listed)
}
