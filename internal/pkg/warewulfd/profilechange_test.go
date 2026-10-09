package warewulfd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/overlay"
	"github.com/warewulf/warewulf/internal/pkg/testenv"
	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/stretchr/testify/assert"
)

// profileChangeTestEnv builds a test environment containing one valid
// image source ("testimg") and four site overlays:
//   - so-a:     usable system overlay
//   - ro-a:     usable runtime overlay
//   - so-bad:   system overlay with a template that fails to parse
//   - so-fixed: same name slot is produced by rewriting so-bad content
func profileChangeTestEnv(t *testing.T, nodesConf string) *testenv.TestEnv {
	t.Helper()
	SetNoDaemon()
	env := testenv.New(t)
	env.MkdirAll("var/lib/warewulf/chroots/testimg/rootfs")
	env.WriteFile("var/lib/warewulf/overlays/so-a/rootfs/etc/hello", "system")
	env.WriteFile("var/lib/warewulf/overlays/ro-a/rootfs/run.txt", "runtime")
	env.WriteFile("var/lib/warewulf/overlays/so-bad/rootfs/bad.ww", "{{ broken template ")
	env.WriteFile("/etc/warewulf/nodes.conf", nodesConf)
	resetProfileChangeStore()
	assert.NoError(t, LoadNodeDB())

	t.Cleanup(func() {
		publishConfigBytes = defaultPublishConfigBytes
		reloadDaemonDB = Reload
		buildNodeOverlay = overlay.BuildOverlay
		daemonReloadExternal = DaemonReload
	})
	return env
}

func overlayImagePath(env *testenv.TestEnv, nodeID, context string) string {
	return env.GetPath(path.Join("srv/warewulf/overlays", nodeID, "__"+strings.ToUpper(context)+"__.img"))
}

func sha256Sum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestProfileChangeValidationRejectsBadChange(t *testing.T) {
	profileChangeTestEnv(t, `
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
  a:
    profiles:
      - base
nodes:
  n1:
    profiles:
      - a
`)

	original, err := os.ReadFile(warewulfconf.Get().Paths.NodesConf())
	assert.NoError(t, err)

	t.Run("missing overlay on changed profile", func(t *testing.T) {
		prev, err := node.New()
		assert.NoError(t, err)
		next, err := node.Parse([]byte(`
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - ghost
    runtime overlay:
      - ro-a
  a:
    profiles:
      - base
nodes:
  n1:
    profiles:
      - a
`))
		assert.NoError(t, err)
		err = ApplyProfileChangeDaemon([]string{"base"}, &prev, &next)
		assert.ErrorIs(t, err, ErrInvalidProfileChange)
		assert.Contains(t, err.Error(), "ghost")

		persisted, rerr := os.ReadFile(warewulfconf.Get().Paths.NodesConf())
		assert.NoError(t, rerr)
		assert.Equal(t, original, persisted)
		_, statErr := os.Stat(profileChangeJournalPath())
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("missing image inherited through nested profiles", func(t *testing.T) {
		prev, err := node.New()
		assert.NoError(t, err)
		next, err := node.Parse([]byte(`
nodeprofiles:
  base:
    image name: no-such-image
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
  a:
    profiles:
      - base
nodes:
  n1:
    profiles:
      - a
`))
		assert.NoError(t, err)
		err = ApplyProfileChangeDaemon([]string{"base"}, &prev, &next)
		assert.ErrorIs(t, err, ErrInvalidProfileChange)
		assert.Contains(t, err.Error(), "no-such-image")
	})

	t.Run("profile cycle", func(t *testing.T) {
		prev, err := node.New()
		assert.NoError(t, err)
		next, err := node.Parse([]byte(`
nodeprofiles:
  base:
    profiles:
      - a
  a:
    profiles:
      - base
nodes:
  n1:
    profiles:
      - a
`))
		assert.NoError(t, err)
		err = ApplyProfileChangeDaemon([]string{"base"}, &prev, &next)
		var cycleErr *node.ProfileCycleError
		assert.ErrorAs(t, err, &cycleErr)
		_, statErr := os.Stat(profileChangeJournalPath())
		assert.True(t, os.IsNotExist(statErr))
	})
}

func TestProfileChangePublishFailureRecovery(t *testing.T) {
	env := profileChangeTestEnv(t, `
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    kernel:
      args:
        - old
nodes:
  n1:
    profiles:
      - base
`)

	prev, err := node.New()
	assert.NoError(t, err)
	next, err := node.Parse([]byte(`
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    kernel:
      args:
        - new
nodes:
  n1:
    profiles:
      - base
`))
	assert.NoError(t, err)

	oldConf, err := os.ReadFile(warewulfconf.Get().Paths.NodesConf())
	assert.NoError(t, err)

	failures := 1
	publishConfigBytes = func(content []byte) error {
		if failures > 0 {
			failures--
			return errors.New("injected publish failure")
		}
		return defaultPublishConfigBytes(content)
	}

	err = ApplyProfileChangeDaemon([]string{"base"}, &prev, &next)
	assert.Error(t, err)

	// record stays at publish, old config readable
	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Len(t, records, 1)
	assert.Equal(t, phasePublish, records[0].Phase)
	onDisk, err := os.ReadFile(warewulfconf.Get().Paths.NodesConf())
	assert.NoError(t, err)
	assert.Equal(t, oldConf, onDisk)
	_, err = node.Parse(onDisk)
	assert.NoError(t, err)

	// recover: publish + adopt + builds complete
	publishConfigBytes = defaultPublishConfigBytes
	assert.NoError(t, RecoverProfileChanges(context.Background()))

	published, err := os.ReadFile(warewulfconf.Get().Paths.NodesConf())
	assert.NoError(t, err)
	assert.Contains(t, string(published), "new")
	assert.FileExists(t, overlayImagePath(env, "n1", "system"))
	assert.FileExists(t, overlayImagePath(env, "n1", "runtime"))
	records, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
	_, err = os.Stat(profileChangeJournalPath())
	assert.True(t, os.IsNotExist(err))
}

func TestProfileChangeAdoptFailureRecovery(t *testing.T) {
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
    network devices:
      default:
        hwaddr: 00:00:00:aa:aa:aa
`)

	prev, err := node.New()
	assert.NoError(t, err)
	next, err := node.Parse([]byte(`
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    tags:
      role: compute
nodes:
  n1:
    profiles:
      - base
    network devices:
      default:
        hwaddr: 00:00:00:aa:aa:aa
`))
	assert.NoError(t, err)

	reloadDaemonDB = func() error { return errors.New("injected reload failure") }
	err = ApplyProfileChangeDaemon([]string{"base"}, &prev, &next)
	assert.Error(t, err)

	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Len(t, records, 1)
	assert.Equal(t, phaseAdopt, records[0].Phase)

	// the in-memory DB has not adopted the new revision yet
	n, err := GetNode("00:00:00:aa:aa:aa")
	assert.NoError(t, err)
	assert.NotEqual(t, "compute", n.Tags["role"])

	// recovery performs the real reload and proceeds to overlay builds
	reloadDaemonDB = Reload
	assert.NoError(t, RecoverProfileChanges(context.Background()))

	n, err = GetNode("00:00:00:aa:aa:aa")
	assert.NoError(t, err)
	assert.Equal(t, "compute", n.Tags["role"])
	assert.FileExists(t, overlayImagePath(env, "n1", "system"))
	assert.FileExists(t, overlayImagePath(env, "n1", "runtime"))
	records, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
}

func TestProfileChangeOverlayFailureIsolation(t *testing.T) {
	env := profileChangeTestEnv(t, `
nodeprofiles:
  pok:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    kernel:
      args:
        - old
  pbad:
    image name: testimg
    system overlay:
      - so-bad
    runtime overlay:
      - ro-a
    kernel:
      args:
        - old
nodes:
  n1:
    profiles:
      - pbad
  n2:
    profiles:
      - pok
`)

	prev, err := node.New()
	assert.NoError(t, err)
	next, err := node.Parse([]byte(`
nodeprofiles:
  pok:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    kernel:
      args:
        - new
  pbad:
    image name: testimg
    system overlay:
      - so-bad
    runtime overlay:
      - ro-a
    kernel:
      args:
        - new
nodes:
  n1:
    profiles:
      - pbad
  n2:
    profiles:
      - pok
`))
	assert.NoError(t, err)

	assert.NoError(t, ApplyProfileChangeDaemon([]string{"pbad", "pok"}, &prev, &next))

	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Len(t, records, 1)
	rec := records[0]
	assert.Equal(t, phaseOverlays, rec.Phase)
	states := map[string]*affectedNodeState{}
	for i := range rec.Nodes {
		states[rec.Nodes[i].Node] = &rec.Nodes[i]
	}
	assert.Len(t, states, 2)
	assert.Equal(t, stFailed, states["n1"].System.Status)
	assert.NotEmpty(t, states["n1"].System.Error)
	assert.Equal(t, stDone, states["n1"].Runtime.Status)
	assert.Equal(t, stDone, states["n2"].System.Status)
	assert.Equal(t, stDone, states["n2"].Runtime.Status)
	assert.FileExists(t, overlayImagePath(env, "n2", "system"))
	assert.FileExists(t, overlayImagePath(env, "n1", "runtime"))

	// fix the broken template and retry: everything converges and the
	// journal is pruned
	env.WriteFile("var/lib/warewulf/overlays/so-bad/rootfs/bad.ww", "fixed content")
	assert.NoError(t, RecoverProfileChanges(context.Background()))
	records, err = profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
	assert.FileExists(t, overlayImagePath(env, "n1", "system"))
}

func TestProfileChangeRestartResume(t *testing.T) {
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
  n-gone:
    profiles:
      - base
`)

	// simulate a crash mid-overlays: journal on disk, no images yet,
	// n-gone has since been removed from nodes.conf
	currentNodesConf := `
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
`
	env.WriteFile("/etc/warewulf/nodes.conf", currentNodesConf)
	payload := []byte(currentNodesConf)
	hash := sha256Sum(payload)
	now := time.Now()
	rec := &profileChangeRecord{
		ID:         "crashed-1",
		Roots:      []string{"base"},
		StartedAt:  now,
		UpdatedAt:  now,
		Phase:      phaseOverlays,
		ConfigYAML: string(payload),
		ConfigHash: hash,
		AutoBuild:  true,
		Nodes: []affectedNodeState{
			{
				Node:    "n1",
				System:  overlayBuildState{Status: stPending, UpdatedAt: now},
				Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
			},
			{
				Node:    "n-gone",
				System:  overlayBuildState{Status: stPending, UpdatedAt: now},
				Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
			},
		},
	}
	resetProfileChangeStore()
	assert.NoError(t, profileChanges.add(rec))
	assert.NoError(t, LoadNodeDB())

	assert.NoError(t, RecoverProfileChanges(context.Background()))

	assert.FileExists(t, overlayImagePath(env, "n1", "system"))
	assert.FileExists(t, overlayImagePath(env, "n1", "runtime"))
	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
	_, err = os.Stat(profileChangeJournalPath())
	assert.True(t, os.IsNotExist(err))
}

func TestProfileChangeManualBuildSatisfiesRecovery(t *testing.T) {
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
	payload := env.ReadFile("/etc/warewulf/nodes.conf")
	hash := sha256Sum([]byte(payload))
	now := time.Now()
	rec := &profileChangeRecord{
		ID: "manual-1", Roots: []string{"base"}, StartedAt: now, UpdatedAt: now,
		Phase: phaseOverlays, ConfigYAML: payload, ConfigHash: hash, AutoBuild: true,
		Nodes: []affectedNodeState{{
			Node:    "n1",
			System:  overlayBuildState{Status: stPending, UpdatedAt: now},
			Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
		}},
	}
	resetProfileChangeStore()
	assert.NoError(t, profileChanges.add(rec))

	// emulate `wwctl overlay build`: an image newer than nodes.conf
	// (testenv pins nodes.conf mtime to 2006) without invoking the builder
	buildCalls := 0
	buildNodeOverlay = func(node.Node, []node.Node, string, []string) error {
		buildCalls++
		return nil
	}
	imgDir := env.GetPath("srv/warewulf/overlays/n1")
	assert.NoError(t, os.MkdirAll(imgDir, 0o755))
	assert.NoError(t, os.WriteFile(path.Join(imgDir, "__SYSTEM__.img"), []byte("manual"), 0o644))
	assert.NoError(t, os.WriteFile(path.Join(imgDir, "__RUNTIME__.img"), []byte("manual"), 0o644))

	assert.NoError(t, RecoverProfileChanges(context.Background()))
	assert.Equal(t, 0, buildCalls)
	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
}

// After the daemon warmed an empty journal cache, a separate wwctl
// process publishes a phase-adopt record + nodes.conf and signals
// SIGHUP: recovery must observe the foreign record, adopt the payload
// and finish its overlay builds.
func TestProfileChangeExternalProcessRecordRecovered(t *testing.T) {
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

	// the CLI-style next configuration, written straight to disk
	nextConf := `
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    tags:
      role: compute
nodes:
  n1:
    profiles:
      - base
`
	env.WriteFile("/etc/warewulf/nodes.conf", nextConf)
	payload := []byte(nextConf)
	now := time.Now()
	foreign := &profileChangeJournal{
		Version: journalVersion,
		Changes: []*profileChangeRecord{{
			ID: "foreign-adopt", Roots: []string{"base"}, StartedAt: now, UpdatedAt: now,
			Phase: phaseAdopt, ConfigYAML: string(payload), ConfigHash: sha256Sum(payload), AutoBuild: true,
			Nodes: []affectedNodeState{{
				Node:    "n1",
				System:  overlayBuildState{Status: stPending, UpdatedAt: now},
				Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
			}},
		}},
	}
	// warm an empty cache like a daemon that started before the CLI ran
	listed, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, listed)

	// publish the journal exactly as a separate wwctl process would:
	// temp-file + rename with no involvement of the in-process store
	doc, err := json.MarshalIndent(foreign, "", "  ")
	assert.NoError(t, err)
	assert.NoError(t, os.MkdirAll(path.Dir(profileChangeJournalPath()), 0o755))
	assert.NoError(t, util.AtomicWriteFile(profileChangeJournalPath(), append(doc, '\n'), 0o600))

	assert.NoError(t, RecoverProfileChanges(context.Background()))

	// adopt adopted the foreign payload: the new tag is served to nodes
	registry, err := node.New()
	assert.NoError(t, err)
	merged, _, err := registry.MergeNode("n1")
	assert.NoError(t, err)
	assert.Equal(t, "compute", merged.Tags["role"])
	assert.FileExists(t, overlayImagePath(env, "n1", "system"))
	assert.FileExists(t, overlayImagePath(env, "n1", "runtime"))
	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
}

// Items already persisted as done (a build completed just before a
// crash) must not be rebuilt on recovery; pending items still are.
func TestProfileChangeDoneItemsNotRebuilt(t *testing.T) {
	env := profileChangeTestEnv(t, `
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
nodes:
  n-done:
    profiles:
      - base
  n-pending:
    profiles:
      - base
`)
	conf := env.ReadFile("/etc/warewulf/nodes.conf")
	now := time.Now()
	rec := &profileChangeRecord{
		ID: "mixed-1", Roots: []string{"base"}, StartedAt: now, UpdatedAt: now,
		Phase: phaseOverlays, ConfigYAML: conf, ConfigHash: sha256Sum([]byte(conf)), AutoBuild: true,
		Nodes: []affectedNodeState{
			{
				Node:    "n-done",
				System:  overlayBuildState{Status: stDone, UpdatedAt: now},
				Runtime: overlayBuildState{Status: stDone, UpdatedAt: now},
			},
			{
				Node:    "n-pending",
				System:  overlayBuildState{Status: stPending, UpdatedAt: now},
				Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
			},
		},
	}
	resetProfileChangeStore()
	assert.NoError(t, profileChanges.add(rec))

	built := []string{}
	origin := buildNodeOverlay
	buildNodeOverlay = func(n node.Node, _ []node.Node, context string, _ []string) error {
		built = append(built, n.Id()+"/"+context)
		return origin(n, nil, context, nil)
	}
	t.Cleanup(func() { buildNodeOverlay = origin })

	assert.NoError(t, RecoverProfileChanges(context.Background()))
	assert.ElementsMatch(t, []string{"n-pending/system", "n-pending/runtime"}, built)
	assert.FileExists(t, overlayImagePath(env, "n-pending", "system"))
	_, err := os.Stat(overlayImagePath(env, "n-done", "system"))
	assert.True(t, os.IsNotExist(err))
}

// Overlay builds happen only for nodes whose merged result changed:
// an explicit tag override and a negated profile exclude the node even
// though it lists the changed profile.
func TestProfileChangeBuildCountExcludesOverrideAndNegation(t *testing.T) {
	profileChangeTestEnv(t, `
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
  a:
    profiles:
      - base
nodes:
  n-changed:
    profiles:
      - a
  n-override:
    profiles:
      - base
    tags:
      role: compute
  n-negated:
    profiles:
      - a
      - "~base"
`)

	built := map[string]int{}
	origin := buildNodeOverlay
	buildNodeOverlay = func(n node.Node, all []node.Node, context string, names []string) error {
		built[n.Id()+"/"+context]++
		return origin(n, all, context, names)
	}
	t.Cleanup(func() { buildNodeOverlay = origin })

	prev, err := node.New()
	assert.NoError(t, err)
	next, err := node.Parse([]byte(`
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    tags:
      role: compute
  a:
    profiles:
      - base
nodes:
  n-changed:
    profiles:
      - a
  n-override:
    profiles:
      - base
    tags:
      role: compute
  n-negated:
    profiles:
      - a
      - "~base"
`))
	assert.NoError(t, err)
	assert.NoError(t, ApplyProfileChangeDaemon([]string{"base"}, &prev, &next))

	assert.Equal(t, 1, built["n-changed/system"])
	assert.Equal(t, 1, built["n-changed/runtime"])
	assert.Zero(t, built["n-override/system"])
	assert.Zero(t, built["n-negated/system"])
}

func TestProfileChangeAutobuildDisabled(t *testing.T) {
	env := profileChangeTestEnv(t, `
nodeprofiles:
  base:
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
nodes:
  n1:
    profiles:
      - base
`)
	disabled := false
	warewulfconf.Get().Warewulf.AutobuildOverlaysP = &disabled

	buildCalls := 0
	buildNodeOverlay = func(node.Node, []node.Node, string, []string) error {
		buildCalls++
		return errors.New("autobuild disabled must not build")
	}

	prev, err := node.New()
	assert.NoError(t, err)
	next, err := node.Parse([]byte(`
nodeprofiles:
  base:
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    tags:
      role: compute
nodes:
  n1:
    profiles:
      - base
`))
	assert.NoError(t, err)

	assert.NoError(t, ApplyProfileChangeDaemon([]string{"base"}, &prev, &next))
	assert.Equal(t, 0, buildCalls)
	_, err = os.Stat(overlayImagePath(env, "n1", "system"))
	assert.True(t, os.IsNotExist(err))
	records, err := profileChanges.list()
	assert.NoError(t, err)
	assert.Empty(t, records)
}
