package warewulfd

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/stretchr/testify/assert"
)

// TestKickProfileChangeWorkerDrainsJournal drives the asynchronous
// trigger used by RunServer on startup and by the SIGHUP handler: a
// journaled overlays-phase record must be completed by the background
// worker without a synchronous recovery call.
func TestKickProfileChangeWorkerDrainsJournal(t *testing.T) {
	env := profileChangeTestEnv(t, `
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
nodes:
  n1:
    profiles:
      - base
`)

	// exercise the daemon (non-nodaemon) code path for the async trigger
	prevDaemon := nodaemon
	nodaemon = false
	t.Cleanup(func() { nodaemon = prevDaemon })

	payload := env.ReadFile("/etc/warewulf/nodes.conf")
	hash := sha256Sum([]byte(payload))
	now := time.Now()
	rec := &profileChangeRecord{
		ID: "worker-1", Roots: []string{"base"}, StartedAt: now, UpdatedAt: now,
		Phase: phaseOverlays, ConfigYAML: payload, ConfigHash: hash, AutoBuild: true,
		Nodes: []affectedNodeState{{
			Node:    "n1",
			System:  overlayBuildState{Status: stPending, UpdatedAt: now},
			Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
		}},
	}
	assert.NoError(t, profileChanges.add(rec))
	assert.NoError(t, LoadNodeDB())

	KickProfileChangeWorker()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		records, err := profileChanges.list()
		assert.NoError(t, err)
		if len(records) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
	assert.FileExists(t, overlayImagePath(env, "n1", "system"))
	assert.FileExists(t, overlayImagePath(env, "n1", "runtime"))
	_, err = os.Stat(profileChangeJournalPath())
	assert.True(t, os.IsNotExist(err))
}

// A corrupt journal must not prevent the daemon from reloading and
// serving; recovery reports the parse error and skips the pass.
func TestProfileChangeCorruptJournalRecovery(t *testing.T) {
	profileChangeTestEnv(t, `
nodeprofiles:
  default: {}
nodes:
  n1: {}
`)
	assert.NoError(t, os.MkdirAll(path.Dir(profileChangeJournalPath()), 0o755))
	assert.NoError(t, util.AtomicWriteFile(profileChangeJournalPath(), []byte("{not json"), 0o600))

	assert.Error(t, RecoverProfileChanges(context.Background()))

	// the daemon itself keeps working
	assert.NoError(t, Reload())

	// once the stale journal is removed, recovery is clean
	assert.NoError(t, os.Remove(profileChangeJournalPath()))
	resetProfileChangeStore()
	assert.NoError(t, RecoverProfileChanges(context.Background()))
}
