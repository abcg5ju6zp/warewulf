package warewulfd

// Profile updates are coordinated as durable, recoverable transactions.
//
// A profile change moves through three persisted phases:
//
//	publish  -> the new nodes.conf payload is atomically renamed into place
//	adopt    -> the daemon re-reads nodes.conf (in-process Reload or SIGHUP)
//	overlays -> system/runtime overlay images are rebuilt per affected node
//
// Once every overlay item is done the record is pruned from the journal.
// A failure leaves the record in its current phase with an error message.
// The single background worker (or an explicit recovery run after daemon
// start, SIGHUP, or a nodaemon CLI invocation) resumes every unfinished
// record without redoing completed work.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/image"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/overlay"
	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// ErrInvalidProfileChange wraps every validation problem that must reject
// a profile update before anything is written to disk.
var ErrInvalidProfileChange = errors.New("invalid profile change")

// ErrDaemonReload marks the situation where nodes.conf was published and
// journaled but the external warewulfd could not be signaled to reload.
// The change is safely durable: the daemon resumes it on its next reload
// or startup, so callers may treat this as a warning.
var ErrDaemonReload = errors.New("warewulfd could not be reloaded")

var (
	// commitLock serializes the short submit/adopt critical sections
	// within one process so concurrent updates cannot interleave their
	// publish/adopt phases.
	commitLock sync.Mutex

	// workerRun ensures at most one recovery/worker pass at a time.
	workerRun sync.Mutex

	// Indirected seams for failure injection in tests.
	publishConfigBytes   = defaultPublishConfigBytes
	reloadDaemonDB       = Reload
	buildNodeOverlay     = overlay.BuildOverlay
	daemonReloadExternal = DaemonReload
)

// defaultPublishConfigBytes atomically writes a full nodes.conf payload.
func defaultPublishConfigBytes(content []byte) error {
	confPath := warewulfconf.Get().Paths.NodesConf()
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		return fmt.Errorf("could not create configuration directory: %w", err)
	}
	if err := util.AtomicWriteFile(confPath, content, 0o644); err != nil {
		return fmt.Errorf("could not publish nodes configuration: %w", err)
	}
	return nil
}

// validateImageOverlays collects problems with one profile/node view:
// a non-empty image must exist as an image source and every listed
// overlay must resolve.
func validateImageOverlays(imageName string, systemOverlays, runtimeOverlays []string, where string, problems *[]string) {
	if imageName != "" && !image.ValidSource(imageName) {
		*problems = append(*problems, fmt.Sprintf("%s: image '%s' does not exist", where, imageName))
	}
	for _, name := range systemOverlays {
		if _, err := overlay.Get(name); err != nil {
			*problems = append(*problems, fmt.Sprintf("%s: system overlay '%s' does not exist", where, name))
		}
	}
	for _, name := range runtimeOverlays {
		if _, err := overlay.Get(name); err != nil {
			*problems = append(*problems, fmt.Sprintf("%s: runtime overlay '%s' does not exist", where, name))
		}
	}
}

// validateProfileChange verifies the next registry is safe to publish and
// returns the precisely affected node IDs (merged result changed and
// within the reverse dependency closure of roots).
//
// Only references *introduced* by this change are required to resolve:
// an update cannot be blocked by an image/overlay/profile reference that
// was already dangling before it (e.g. editing an unrelated field while
// an image is not yet imported). Cycles are rejected when the previously
// acyclic graph becomes cyclic, while edits that leave or repair an
// already cyclic graph are allowed.
func validateProfileChange(roots []string, prev, next *node.NodesYaml) (affected []string, err error) {
	if cycleErr := next.CheckProfileCycles(); cycleErr != nil && prev.CheckProfileCycles() == nil {
		// wrap both sentinels so callers can match ErrInvalidProfileChange
		// and errors.As the concrete ProfileCycleError.
		return nil, fmt.Errorf("%w: %w", ErrInvalidProfileChange, cycleErr)
	}

	var problems []string
	for _, root := range roots {
		nextProfile, ok := next.NodeProfiles[root]
		if !ok {
			return nil, fmt.Errorf("%w: profile '%s' does not exist", ErrInvalidProfileChange, root)
		}
		prevProfile, _ := prev.NodeProfiles[root]

		prevRefs := map[string]bool{}
		if prevProfile != nil {
			for _, ref := range prevProfile.Profiles {
				prevRefs[ref] = true
			}
		}
		for _, ref := range nextProfile.Profiles {
			if strings.HasPrefix(ref, "~") || prevRefs[ref] {
				continue
			}
			if _, ok := next.NodeProfiles[ref]; !ok {
				problems = append(problems, fmt.Sprintf("profile '%s' references missing profile '%s'", root, ref))
			}
		}

		prevImage := ""
		var prevSystem, prevRuntime []string
		if prevProfile != nil {
			prevImage = prevProfile.ImageName
			prevSystem = prevProfile.SystemOverlay
			prevRuntime = prevProfile.RuntimeOverlay
		}
		where := fmt.Sprintf("profile '%s'", root)
		if nextProfile.ImageName != "" && nextProfile.ImageName != prevImage {
			validateImageOverlays(nextProfile.ImageName, nil, nil, where, &problems)
		}
		validateNewOverlays(nextProfile.SystemOverlay, prevSystem, "system", where, &problems)
		validateNewOverlays(nextProfile.RuntimeOverlay, prevRuntime, "runtime", where, &problems)
	}

	affected = next.AffectedNodes(prev, roots)
	for _, nodeID := range affected {
		mergedNext, _, mergeErr := next.MergeNode(nodeID)
		if mergeErr != nil {
			problems = append(problems, fmt.Sprintf("node '%s': %v", nodeID, mergeErr))
			continue
		}
		mergedPrev, _, mergeErr := prev.MergeNode(nodeID)
		if mergeErr != nil {
			// node only present in next: all of its references are new
			mergedPrev = node.Node{}
		}
		where := fmt.Sprintf("node '%s'", nodeID)
		if mergedNext.ImageName != "" && mergedNext.ImageName != mergedPrev.ImageName {
			validateImageOverlays(mergedNext.ImageName, nil, nil, where, &problems)
		}
		validateNewOverlays(mergedNext.SystemOverlay, mergedPrev.SystemOverlay, "system", where, &problems)
		validateNewOverlays(mergedNext.RuntimeOverlay, mergedPrev.RuntimeOverlay, "runtime", where, &problems)
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%w:\n  - %s", ErrInvalidProfileChange, strings.Join(problems, "\n  - "))
	}
	return affected, nil
}

// validateNewOverlays reports overlays present in current but absent from
// previous as missing references.
func validateNewOverlays(current, previous []string, kind, where string, problems *[]string) {
	had := map[string]bool{}
	for _, name := range previous {
		had[name] = true
	}
	for _, name := range current {
		if had[name] {
			continue
		}
		if _, err := overlay.Get(name); err != nil {
			*problems = append(*problems, fmt.Sprintf("%s: %s overlay '%s' does not exist", where, kind, name))
		}
	}
}

// persistRecord persists mutated record state and reports journal I/O
// failures. Such failures do not change the primary outcome (the next
// recovery pass re-derives the state), but they must be visible in logs.
func persistRecord(rec *profileChangeRecord) {
	if err := profileChanges.update(rec); err != nil {
		wwlog.Error("Profile change %s: could not persist journal state: %s", rec.ID, err)
	}
}

// beginProfileChange validates the change and durably records it in the
// publish phase. No configuration is written yet. The caller must hold
// (or serialize around) commitLock.
func beginProfileChange(roots []string, prev, next *node.NodesYaml) (*profileChangeRecord, error) {
	roots = slices.Compact(slices.Sorted(slices.Values(roots)))
	affected, err := validateProfileChange(roots, prev, next)
	if err != nil {
		return nil, err
	}

	payload, err := next.Dump()
	if err != nil {
		return nil, fmt.Errorf("could not serialize nodes configuration: %w", err)
	}
	hash := sha256.Sum256(payload)
	now := time.Now()

	rec := &profileChangeRecord{
		ID:         newProfileChangeID(),
		Roots:      roots,
		StartedAt:  now,
		UpdatedAt:  now,
		Phase:      phasePublish,
		ConfigYAML: string(payload),
		ConfigHash: hex.EncodeToString(hash[:]),
		AutoBuild:  warewulfconf.Get().Warewulf.AutobuildOverlays(),
	}
	for _, nodeID := range affected {
		rec.Nodes = append(rec.Nodes, affectedNodeState{
			Node:    nodeID,
			System:  overlayBuildState{Status: stPending, UpdatedAt: now},
			Runtime: overlayBuildState{Status: stPending, UpdatedAt: now},
		})
	}

	if err := profileChanges.add(rec); err != nil {
		return nil, err
	}
	wwlog.Info("Profile change %s started: roots=[%s], %d affected node(s), autobuild=%v",
		rec.ID, strings.Join(roots, ","), len(rec.Nodes), rec.AutoBuild)
	return rec, nil
}

// publishRecord atomically installs the payload unless nodes.conf already
// matches it, then advances the record to the adopt phase.
func publishRecord(rec *profileChangeRecord) error {
	if rec.Phase != phasePublish {
		return nil
	}
	needPublish := true
	if data, err := os.ReadFile(warewulfconf.Get().Paths.NodesConf()); err == nil {
		current := sha256.Sum256(data)
		if hex.EncodeToString(current[:]) == rec.ConfigHash {
			needPublish = false
			wwlog.Debug("Profile change %s: nodes.conf already at target revision, skipping publish", rec.ID)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not read current nodes.conf: %w", err)
	}
	if needPublish {
		if err := publishConfigBytes([]byte(rec.ConfigYAML)); err != nil {
			rec.PhaseError = err.Error()
			_ = profileChanges.update(rec)
			return err
		}
	}
	rec.Phase = phaseAdopt
	rec.PhaseError = ""
	if err := profileChanges.update(rec); err != nil {
		return err
	}
	wwlog.Info("Profile change %s: nodes.conf published", rec.ID)
	return nil
}

// adoptRecord reloads the in-process node database so the daemon serves
// the new configuration. With autobuild disabled (or no affected nodes)
// the transaction completes here.
func adoptRecord(rec *profileChangeRecord) error {
	if rec.Phase != phaseAdopt {
		return nil
	}
	if err := reloadDaemonDB(); err != nil {
		wrapped := fmt.Errorf("daemon could not adopt new configuration: %w", err)
		rec.PhaseError = wrapped.Error()
		_ = profileChanges.update(rec)
		return wrapped
	}
	rec.PhaseError = ""
	if !rec.AutoBuild || len(rec.Nodes) == 0 {
		if err := profileChanges.remove(rec.ID); err != nil {
			return err
		}
		wwlog.Info("Profile change %s: completed after adopt (autobuild=%v, %d node(s))",
			rec.ID, rec.AutoBuild, len(rec.Nodes))
		return nil
	}
	rec.Phase = phaseOverlays
	if err := profileChanges.update(rec); err != nil {
		return err
	}
	wwlog.Info("Profile change %s: adopted by daemon, rebuilding overlays for %d node(s)",
		rec.ID, len(rec.Nodes))
	return nil
}

// buildOverlayItem builds one (node, context) image and records its
// outcome. An existing image newer than nodes.conf is accepted as
// up to date, letting a manual `wwctl overlay build` satisfy recovery.
func buildOverlayItem(registry *node.NodesYaml, allNodes []node.Node, rec *profileChangeRecord, item *overlayBuildState, nodeID, context string) {
	if item.Status == stDone {
		return
	}
	names := []string{}
	if merged, _, err := registry.MergeNode(nodeID); err == nil {
		if context == "system" {
			names = merged.SystemOverlay
		} else {
			names = merged.RuntimeOverlay
		}
	}

	imagePath := overlay.Image(nodeID, context, nil)
	if util.IsFile(imagePath) && !util.PathIsNewer(imagePath, warewulfconf.Get().Paths.NodesConf()) {
		wwlog.Info("Profile change %s: %s %s overlay already current, skipping build", rec.ID, nodeID, context)
		item.Status = stDone
		item.Error = ""
		item.UpdatedAt = time.Now()
		return
	}

	merged, _, err := registry.MergeNode(nodeID)
	if err != nil {
		item.Status = stFailed
		item.Error = fmt.Sprintf("could not merge node %s: %v", nodeID, err)
		item.Attempts++
		item.UpdatedAt = time.Now()
		return
	}

	item.Attempts++
	wwlog.Info("Profile change %s: building %s overlay for %s (attempt %d)", rec.ID, context, nodeID, item.Attempts)
	if err := buildNodeOverlay(merged, allNodes, context, names); err != nil {
		item.Status = stFailed
		item.Error = err.Error()
		wwlog.Error("Profile change %s: %s %s overlay build failed (attempt %d): %s",
			rec.ID, nodeID, context, item.Attempts, err)
	} else {
		item.Status = stDone
		item.Error = ""
		wwlog.Info("Profile change %s: %s %s overlay rebuilt", rec.ID, nodeID, context)
	}
	item.UpdatedAt = time.Now()
}

// buildRecordOverlays drives the per-node/per-context builds of one
// record and prunes it once every item is done.
func buildRecordOverlays(ctx context.Context, rec *profileChangeRecord) {
	if rec.Phase != phaseOverlays {
		return
	}
	if ctx.Err() != nil {
		return
	}
	registry, err := node.New()
	if err != nil {
		rec.PhaseError = fmt.Sprintf("could not open nodes.conf for overlay rebuilds: %v", err)
		_ = profileChanges.update(rec)
		wwlog.Error("Profile change %s: %s", rec.ID, rec.PhaseError)
		return
	}
	allNodes, err := registry.FindAllNodes()
	if err != nil {
		rec.PhaseError = fmt.Sprintf("could not list nodes for overlay rebuilds: %v", err)
		_ = profileChanges.update(rec)
		wwlog.Error("Profile change %s: %s", rec.ID, rec.PhaseError)
		return
	}
	rec.PhaseError = ""

	allDone := true
	for i := range rec.Nodes {
		nodeState := &rec.Nodes[i]
		if _, err := registry.GetNode(nodeState.Node); err != nil {
			// The node no longer exists: nothing to build for it.
			wwlog.Warn("Profile change %s: node %s no longer exists, marking its builds done",
				rec.ID, nodeState.Node)
			now := time.Now()
			nodeState.System = overlayBuildState{Status: stDone, UpdatedAt: now}
			nodeState.Runtime = overlayBuildState{Status: stDone, UpdatedAt: now}
			_ = profileChanges.update(rec)
			continue
		}
		buildOverlayItem(&registry, allNodes, rec, &nodeState.System, nodeState.Node, "system")
		_ = profileChanges.update(rec)
		if ctx.Err() != nil {
			return
		}
		buildOverlayItem(&registry, allNodes, rec, &nodeState.Runtime, nodeState.Node, "runtime")
		_ = profileChanges.update(rec)
		if nodeState.System.Status != stDone || nodeState.Runtime.Status != stDone {
			allDone = false
		}
	}

	if allDone {
		if err := profileChanges.remove(rec.ID); err != nil {
			wwlog.Error("Profile change %s: could not finalize journal record: %s", rec.ID, err)
			return
		}
		wwlog.Info("Profile change %s: all overlay rebuilds completed", rec.ID)
	} else {
		_ = profileChanges.update(rec)
		wwlog.Warn("Profile change %s: some overlay rebuilds remain pending; recorded for retry", rec.ID)
	}
}

// processProfileChanges drives every pending record forward by one pass.
// Failures stay journaled per phase/item and do not abort other records.
func processProfileChanges(ctx context.Context) error {
	records, err := profileChanges.list()
	if err != nil {
		wwlog.Error("Profile change journal unreadable, skipping recovery: %s", err)
		return err
	}
	if len(records) == 0 {
		return nil
	}

	for _, rec := range records {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// publish/adopt are quick critical sections; serialize them with
		// in-flight submissions.
		commitLock.Lock()
		if rec.Phase == phasePublish {
			if err := publishRecord(rec); err != nil {
				wwlog.Error("Profile change %s: publish phase pending: %s", rec.ID, err)
			}
		}
		if rec.Phase == phaseAdopt {
			if err := adoptRecord(rec); err != nil {
				wwlog.Error("Profile change %s: adopt phase pending: %s", rec.ID, err)
			}
		}
		commitLock.Unlock()

		if rec.Phase == phaseOverlays {
			buildRecordOverlays(ctx, rec)
		}
	}
	return nil
}

// runProfileChangeOnce executes one recovery pass, provided no other pass
// is running; concurrent triggers coalesce.
func runProfileChangeOnce(ctx context.Context) error {
	if !workerRun.TryLock() {
		wwlog.Debug("profile change worker already running, coalescing trigger")
		return nil
	}
	defer workerRun.Unlock()
	return processProfileChanges(ctx)
}

// background worker plumbing
var (
	workerOnce sync.Once
	workerWake chan struct{}
)

func startProfileChangeWorker() {
	workerOnce.Do(func() {
		workerWake = make(chan struct{}, 1)
		go func() {
			for range workerWake {
				if err := runProfileChangeOnce(context.Background()); err != nil {
					wwlog.Debug("profile change worker pass ended: %s", err)
				}
			}
		}()
	})
}

// kickProfileChangeWorker starts (if needed) and wakes the background
// worker. The wake is coalesced: only one pass runs at a time.
func kickProfileChangeWorker() {
	startProfileChangeWorker()
	select {
	case workerWake <- struct{}{}:
	default:
	}
}

// KickProfileChangeWorker is the exported recovery trigger used by the
// daemon server on startup and after SIGHUP-triggered reloads.
func KickProfileChangeWorker() {
	kickProfileChangeWorker()
}

// RecoverProfileChanges performs one synchronous recovery pass. It is
// called on daemon startup, after SIGHUP reloads, and from nodaemon CLI
// invocations so journaled work survives process restarts.
func RecoverProfileChanges(ctx context.Context) error {
	return runProfileChangeOnce(ctx)
}

// ApplyProfileChangeDaemon is the entry point for in-daemon writers (the
// REST API): validate, journal, publish, adopt synchronously, then let
// the background worker rebuild overlays. Under nodaemon (tests and
// daemon-less invocations) the recovery pass runs inline instead.
//
// Validation problems return ErrInvalidProfileChange wrapped; nothing has
// been written in that case. Publish/adopt failures are returned with the
// record retained at the failed phase for retry.
func ApplyProfileChangeDaemon(roots []string, prev, next *node.NodesYaml) error {
	commitLock.Lock()
	rec, err := beginProfileChange(roots, prev, next)
	if err != nil {
		commitLock.Unlock()
		return err
	}
	if err := publishRecord(rec); err != nil {
		commitLock.Unlock()
		return err
	}
	if err := adoptRecord(rec); err != nil {
		commitLock.Unlock()
		return err
	}
	phase := rec.Phase
	commitLock.Unlock()

	if nodaemon {
		// No external worker: finish the overlay phase inline so callers
		// (and tests) observe completed builds.
		return RecoverProfileChanges(context.Background())
	}
	if phase == phaseOverlays {
		kickProfileChangeWorker()
	}
	return nil
}

// ApplyProfileChangeCLI is the entry point for wwctl: validate, journal
// and publish, then ask the external daemon to adopt via service reload.
// The daemon resumes the record through its SIGHUP recovery. With no
// daemon running (nodaemon) adoption and overlay rebuilds happen inline.
func ApplyProfileChangeCLI(roots []string, prev, next *node.NodesYaml) error {
	commitLock.Lock()
	rec, err := beginProfileChange(roots, prev, next)
	if err != nil {
		commitLock.Unlock()
		return err
	}
	if err := publishRecord(rec); err != nil {
		commitLock.Unlock()
		return err
	}
	commitLock.Unlock()

	if nodaemon {
		commitLock.Lock()
		err := adoptRecord(rec)
		commitLock.Unlock()
		if err != nil {
			return err
		}
		return RecoverProfileChanges(context.Background())
	}

	if err := daemonReloadExternal(); err != nil {
		rec.Phase = phaseAdopt
		rec.PhaseError = err.Error()
		_ = profileChanges.update(rec)
		return fmt.Errorf("nodes.conf published but %w: %w", ErrDaemonReload, err)
	}
	wwlog.Info("Profile change %s: warewulfd reload signaled; daemon will rebuild affected node overlays", rec.ID)
	return nil
}
