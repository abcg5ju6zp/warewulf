package warewulfd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/image"
	"github.com/warewulf/warewulf/internal/pkg/kernel"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/overlay"
	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// A boot generation ("代际") is the immutable set of node identity and
// boot assets that a single boot is bound to. It is created when the first
// boot descriptor (iPXE script or GRUB config) is served and persists until
// after every stage of that boot has completed. Every later request of the
// boot resolves only to the complete files recorded in its generation; a
// reload of nodes.conf or a rebuild of the assets can only affect boots that
// start afterwards.
const (
	// bootGenerationTTL bounds generations whose boot has not completed.
	bootGenerationTTL = 24 * time.Hour
	// bootGenerationGraceTTL bounds a generation after every recorded asset
	// has been delivered, allowing for short-term retries of the same boot.
	bootGenerationGraceTTL = time.Hour
	bootGenerationRootName = "boot-generations"
	bootGenerationManifest = "manifest.yaml"
)

// bootGenerationNamePattern matches and validates generation identifiers
// supplied by clients, keeping them path-safe and recognizable in logs.
var bootGenerationNamePattern = regexp.MustCompile(`^wwboot-[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

// bootAssetStages is the complete ordered set of stages recorded in a
// generation.
var bootAssetStages = []string{"kernel", "initramfs", "image", "system", "runtime"}

// gzStages are the stages for which both the plain and the gz-compressed
// form must exist: the boot menus offer compressed and uncompressed entries
// and wwinit/wwclient request compressed files.
var gzStages = map[string]bool{"image": true, "system": true, "runtime": true}

// protectedAssetNames gives the protected file name within a generation
// directory for each stage. The gz form is always the same name with ".gz"
// appended, matching the compression handling in sendResponse.
var protectedAssetNames = map[string]string{
	"kernel":    "kernel",
	"initramfs": "initramfs",
	"image":     "image.img",
	"system":    "system.img",
	"runtime":   "runtime.img",
}

// pinnedAsset records one stage's canonical and generation-private paths and
// the state the protected files must remain in. Sums are recorded for small
// assets (kernel, initramfs); large assets are verified by size and mtime.
type pinnedAsset struct {
	Canonical   string    `yaml:"canonical"`
	Protected   string    `yaml:"protected"`
	GzCanonical string    `yaml:"gz canonical,omitempty"`
	GzProtected string    `yaml:"gz protected,omitempty"`
	Size        int64     `yaml:"size"`
	Mtime       time.Time `yaml:"mtime"`
	Sum         string    `yaml:"sum,omitempty"`
	GzSize      int64     `yaml:"gz size,omitempty"`
	GzMtime     time.Time `yaml:"gz mtime,omitempty"`
}

// bootGenerationData is the on-disk manifest of a boot generation.
type bootGenerationData struct {
	ID        string                 `yaml:"id"`
	NodeID    string                 `yaml:"node id"`
	Hwaddr    string                 `yaml:"hwaddr"`
	CreatedAt time.Time              `yaml:"created at"`
	ExpiresAt time.Time              `yaml:"expires at"`
	Node      node.Node              `yaml:"node"`
	Assets    map[string]pinnedAsset `yaml:"assets"`
	Delivered []string               `yaml:"delivered,omitempty"`
	Invalid   bool                   `yaml:"invalid,omitempty"`
}

// recordedStat is the verified state of one protected file.
type recordedStat struct {
	size  int64
	mtime time.Time
	sum   string
}

// bootGeneration is a live, in-memory view of a generation manifest.
type bootGeneration struct {
	data      bootGenerationData
	dataLock  sync.Mutex // guards Delivered, ExpiresAt, Invalid
	pathIndex map[string]recordedStat
}

// errBootGenerationInvalid is the sentinel returned when a pinned boot can
// no longer be completed from its generation. It directs the node to
// restart; it must never result in a fallback to current assets.
var errBootGenerationInvalid = errors.New("boot generation is invalid or incomplete: the node must restart")

func bootGenerationsDir() string {
	return path.Join(config.Get().Paths.WWProvisiondir, bootGenerationRootName)
}

func (g *bootGeneration) dir() string {
	return path.Join(bootGenerationsDir(), g.data.ID)
}

func (g *bootGeneration) manifestPath() string {
	return path.Join(g.dir(), bootGenerationManifest)
}

func newBootGenerationID() string {
	now := time.Now().UTC()
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("wwboot-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(b))
}

// buildPathIndex indexes every protected file (plain and gz) with the state
// recorded at creation.
func (g *bootGeneration) buildPathIndex() {
	g.pathIndex = make(map[string]recordedStat)
	for _, a := range g.data.Assets {
		g.pathIndex[a.Protected] = recordedStat{size: a.Size, mtime: a.Mtime, sum: a.Sum}
		if a.GzProtected != "" {
			g.pathIndex[a.GzProtected] = recordedStat{size: a.GzSize, mtime: a.GzMtime}
		}
	}
}

// resolveCanonicalAssets resolves the canonical file paths of every boot
// asset for n, applying the same rules as the individual stage handlers.
func resolveCanonicalAssets(n node.Node) (map[string]pinnedAsset, error) {
	assets := make(map[string]pinnedAsset)
	if !n.Valid() {
		return nil, fmt.Errorf("no valid node")
	}
	if n.ImageName == "" {
		return nil, fmt.Errorf("no image defined for node %s", n.Id())
	}
	k := kernel.FromNode(&n)
	if k == nil {
		return nil, fmt.Errorf("no kernel found for node %s", n.Id())
	}
	kernelPath := k.FullPath()
	if kernelPath == "" {
		return nil, fmt.Errorf("no kernel path found for node %s", n.Id())
	}
	assets["kernel"] = pinnedAsset{Canonical: kernelPath}

	kver := k.Version()
	if kver == "" {
		return nil, fmt.Errorf("unable to determine kernel version for node %s", n.Id())
	}
	initramfs := image.FindInitramfs(n.ImageName, kver)
	if initramfs == nil {
		return nil, fmt.Errorf("no initramfs found for kernel %s in image %s", kver, n.ImageName)
	}
	assets["initramfs"] = pinnedAsset{Canonical: initramfs.FullPath()}
	assets["image"] = pinnedAsset{Canonical: image.ImageFile(n.ImageName)}
	assets["system"] = pinnedAsset{Canonical: overlay.Image(n.Id(), "system", nil)}
	assets["runtime"] = pinnedAsset{Canonical: overlay.Image(n.Id(), "runtime", nil)}
	return assets, nil
}

// copyProtectedFile makes a standalone copy of src at dst, preserving mode,
// ownership and modification time.
func copyProtectedFile(src, dst string) error {
	if err := util.CopyFile(src, dst); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// protectAsset materializes src into the generation-private path dst.
//
// A hardlink is preferred: the provision image and overlay writers replace
// their outputs atomically, so the linked inode survives a rebuild while a
// boot is in progress, without copying large files. Kernel and initramfs
// files are copied (copyOnly), as chroot sources may be overwritten in
// place during image import. A cross-device hardlink failure falls back to a
// full copy.
func protectAsset(src, dst string, copyOnly bool) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("could not create generation asset directory: %w", err)
	}
	if !copyOnly {
		if err := os.Link(src, dst); err == nil {
			return nil
		} else {
			wwlog.Verbose("hardlink %s -> %s failed (%s): copying file", src, dst, err)
		}
	}
	return copyProtectedFile(src, dst)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// recordFileStat stats p and returns its size and mtime, and a SHA-256 sum
// when withSum is set.
func recordFileStat(p string, withSum bool) (size int64, mtime time.Time, sum string, err error) {
	info, err := os.Stat(p)
	if err != nil {
		return
	}
	if !info.Mode().IsRegular() {
		err = fmt.Errorf("not a regular file: %s", p)
		return
	}
	size = info.Size()
	mtime = info.ModTime()
	if withSum {
		sum, err = fileSHA256(p)
	}
	return
}

// pinBootGeneration creates and persists the generation for one boot. It
// resolves every asset, ensures system/runtime overlay images are present
// (preserving the autobuild semantics), requires the complete set of plain
// and compressed files, and materializes generation-private protected
// copies. The returned generation is registered and immediately usable.
func pinBootGeneration(rinfo parsedRequest, n node.Node, autobuildOverlays bool) (*bootGeneration, error) {
	var err error
	// New builds must not disturb boots already in progress; reap expired
	// generations before allocating a new one.
	if sweepErr := sweepBootGenerations(); sweepErr != nil {
		wwlog.Warn("error while sweeping boot generations: %s", sweepErr)
	}

	assets, err := resolveCanonicalAssets(n)
	if err != nil {
		return nil, err
	}

	// Ensure overlay images are built, using the same build conditions as
	// the overlay stage handlers.
	for _, stage := range []string{"system", "runtime"} {
		overlayFile, buildErr := getOverlayFile(n, stage, autobuildOverlays)
		if buildErr != nil {
			return nil, buildErr
		}
		a := assets[stage]
		a.Canonical = overlayFile
		assets[stage] = a
	}

	now := time.Now()
	gen := &bootGeneration{
		data: bootGenerationData{
			ID:        newBootGenerationID(),
			NodeID:    n.Id(),
			Hwaddr:    rinfo.hwaddr,
			CreatedAt: now,
			ExpiresAt: now.Add(bootGenerationTTL),
			Node:      n,
			Assets:    make(map[string]pinnedAsset),
		},
	}
	assetsDir := filepath.Join(gen.dir(), "assets")

	// Remove a partial generation if pinning fails below.
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(gen.dir())
		}
	}()

	for _, stage := range bootAssetStages {
		a := assets[stage]
		if !util.IsFile(a.Canonical) {
			return nil, fmt.Errorf("required boot asset missing: %s", a.Canonical)
		}
		dst := filepath.Join(assetsDir, protectedAssetNames[stage])
		// chroot-hosted files are copied; provision files are hardlinked.
		copyOnly := stage == "kernel" || stage == "initramfs"
		if err = protectAsset(a.Canonical, dst, copyOnly); err != nil {
			return nil, err
		}
		size, mtime, sum, statErr := recordFileStat(dst, copyOnly)
		if statErr != nil {
			return nil, statErr
		}
		a.Protected = dst
		a.Size = size
		a.Mtime = mtime
		a.Sum = sum

		if gzStages[stage] {
			a.GzCanonical = a.Canonical + ".gz"
			if !util.IsFile(a.GzCanonical) {
				return nil, fmt.Errorf("required compressed boot asset missing: %s", a.GzCanonical)
			}
			a.GzProtected = dst + ".gz"
			if err = protectAsset(a.GzCanonical, a.GzProtected, false); err != nil {
				return nil, err
			}
			gzSize, gzMtime, _, statErr := recordFileStat(a.GzProtected, false)
			if statErr != nil {
				return nil, statErr
			}
			a.GzSize = gzSize
			a.GzMtime = gzMtime
		}
		gen.data.Assets[stage] = a
	}

	if err = gen.persist(); err != nil {
		return nil, err
	}
	gen.buildPathIndex()
	bootGenerations.add(gen)
	complete = true
	return gen, nil
}

// persist writes the generation manifest atomically.
func (g *bootGeneration) persist() error {
	data, err := yaml.Marshal(&g.data)
	if err != nil {
		return fmt.Errorf("could not marshal boot generation %s: %w", g.data.ID, err)
	}
	if err = os.MkdirAll(g.dir(), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(g.dir(), ".manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, g.manifestPath())
}

// verifyProtectedFile confirms that the protected file p still exists and
// matches the state recorded at generation creation. Any discrepancy means
// the generation cannot complete the boot.
func (g *bootGeneration) verifyProtectedFile(p string) error {
	want, ok := g.pathIndex[p]
	if !ok {
		return fmt.Errorf("%w (unknown generation file: %s)", errBootGenerationInvalid, p)
	}
	info, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w (missing file: %s)", errBootGenerationInvalid, p)
		}
		return err
	}
	if info.Size() != want.size || !info.ModTime().Equal(want.mtime) {
		return fmt.Errorf("%w (file changed: %s)", errBootGenerationInvalid, p)
	}
	if want.sum != "" {
		sum, sumErr := fileSHA256(p)
		if sumErr != nil {
			return sumErr
		}
		if sum != want.sum {
			return fmt.Errorf("%w (file checksum mismatch: %s)", errBootGenerationInvalid, p)
		}
	}
	return nil
}

// resolveStage returns the protected base path for stage and verifies the
// file that will actually be sent (plain or gz per compress). A non-nil
// error invalidates the generation and requires the node to restart.
func (g *bootGeneration) resolveStage(stage, compress string) (base string, err error) {
	a, ok := g.data.Assets[stage]
	if !ok {
		err = fmt.Errorf("%w (generation has no %s stage)", errBootGenerationInvalid, stage)
		return
	}
	p := a.Protected
	if compress == "gz" {
		if a.GzProtected == "" {
			err = fmt.Errorf("%w (generation has no gz %s)", errBootGenerationInvalid, stage)
			return
		}
		p = a.GzProtected
	} else if compress != "" {
		err = fmt.Errorf("%w (generation has no %s-compressed %s)", errBootGenerationInvalid, compress, stage)
		return
	}
	if err = g.verifyProtectedFile(p); err != nil {
		_ = g.invalidate()
		return "", err
	}
	return a.Protected, nil
}

// markDelivered records that stage has been served. Once every recorded
// asset has been delivered, the generation switches to the grace TTL.
func (g *bootGeneration) markDelivered(stage string) error {
	g.dataLock.Lock()
	defer g.dataLock.Unlock()
	if slices.Contains(g.data.Delivered, stage) {
		return nil
	}
	g.data.Delivered = append(g.data.Delivered, stage)
	sort.Strings(g.data.Delivered)
	if len(g.data.Delivered) == len(g.data.Assets) {
		g.data.ExpiresAt = time.Now().Add(bootGenerationGraceTTL)
	}
	return g.persist()
}

// invalidate marks a generation unusable, removes it from the live registry
// and rewrites the manifest so the sweep reaps it. Protected files are left
// in place: only an explicit sweep (not serve-time failure) removes them.
func (g *bootGeneration) invalidate() error {
	g.dataLock.Lock()
	defer g.dataLock.Unlock()
	if g.data.Invalid {
		return nil
	}
	g.data.Invalid = true
	g.data.ExpiresAt = time.Now()
	bootGenerations.delete(g.data.ID)
	return g.persist()
}

// --- live in-memory store ---

type bootGenerationStore struct {
	lock        sync.RWMutex
	generations map[string]*bootGeneration
}

var bootGenerations = bootGenerationStore{generations: map[string]*bootGeneration{}}

func (s *bootGenerationStore) add(g *bootGeneration) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.generations[g.data.ID] = g
}

func (s *bootGenerationStore) get(id string) (*bootGeneration, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	g, ok := s.generations[id]
	return g, ok
}

func (s *bootGenerationStore) delete(id string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.generations, id)
}

func (s *bootGenerationStore) replace(generations map[string]*bootGeneration) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.generations = generations
}

func (s *bootGenerationStore) list() []*bootGeneration {
	s.lock.RLock()
	defer s.lock.RUnlock()
	out := make([]*bootGeneration, 0, len(s.generations))
	for _, g := range s.generations {
		out = append(out, g)
	}
	return out
}

// lookupBootGeneration validates that bootID refers to a live generation
// bound to hwaddr. It never returns a different or newer generation.
func lookupBootGeneration(bootID, hwaddr string) (*bootGeneration, error) {
	if !bootGenerationNamePattern.MatchString(bootID) {
		return nil, fmt.Errorf("%w (invalid generation id: %s)", errBootGenerationInvalid, bootID)
	}
	g, ok := bootGenerations.get(bootID)
	if !ok {
		return nil, fmt.Errorf("%w (generation %s not found)", errBootGenerationInvalid, bootID)
	}
	g.dataLock.Lock()
	invalid := g.data.Invalid
	expired := time.Now().After(g.data.ExpiresAt)
	boundHwaddr := g.data.Hwaddr
	g.dataLock.Unlock()
	if invalid || expired {
		return nil, fmt.Errorf("%w (generation %s has expired)", errBootGenerationInvalid, bootID)
	}
	if boundHwaddr != hwaddr {
		// Concurrent boots must not share an identity: a generation is
		// bound to exactly the node that started it.
		return nil, fmt.Errorf("%w (generation %s is bound to a different node)", errBootGenerationInvalid, bootID)
	}
	return g, nil
}

// loadBootGeneration reads one generation directory and restores its
// snapshot.
func loadBootGeneration(id string) (*bootGeneration, error) {
	g := &bootGeneration{data: bootGenerationData{ID: id}}
	data, err := os.ReadFile(g.manifestPath())
	if err != nil {
		return nil, err
	}
	if err = yaml.Unmarshal(data, &g.data); err != nil {
		return nil, err
	}
	if g.data.ID != id {
		return nil, fmt.Errorf("manifest id %q does not match directory %q", g.data.ID, id)
	}
	// Restore the node identity lost in the YAML round-trip.
	g.data.Node = node.RestoreNode(g.data.NodeID, g.data.Node)
	g.buildPathIndex()
	return g, nil
}

// LoadBootGenerations loads every persisted generation after a daemon start
// or reload, so interrupted boots can continue against the same files.
// Generations whose protected files are missing or changed are marked invalid
// and never served; loading one bad generation never affects the others.
func LoadBootGenerations() error {
	entries, err := os.ReadDir(bootGenerationsDir())
	if err != nil {
		if os.IsNotExist(err) {
			bootGenerations.replace(map[string]*bootGeneration{})
			return nil
		}
		return err
	}
	generations := make(map[string]*bootGeneration)
	for _, entry := range entries {
		if !entry.IsDir() || !bootGenerationNamePattern.MatchString(entry.Name()) {
			continue
		}
		g, loadErr := loadBootGeneration(entry.Name())
		if loadErr != nil {
			wwlog.Warn("ignoring unreadable boot generation %s: %s", entry.Name(), loadErr)
			continue
		}
		if g.data.Invalid {
			continue
		}
		broken := false
		for _, a := range g.data.Assets {
			if verifyErr := g.verifyProtectedFile(a.Protected); verifyErr != nil {
				wwlog.Warn("boot generation %s incomplete after reload: %s", g.data.ID, verifyErr)
				broken = true
				break
			}
			if a.GzProtected != "" {
				if verifyErr := g.verifyProtectedFile(a.GzProtected); verifyErr != nil {
					wwlog.Warn("boot generation %s incomplete after reload: %s", g.data.ID, verifyErr)
					broken = true
					break
				}
			}
		}
		if broken {
			g.data.Invalid = true
			g.data.ExpiresAt = time.Now()
			if persistErr := g.persist(); persistErr != nil {
				wwlog.Warn("could not mark broken boot generation %s invalid: %s", g.data.ID, persistErr)
			}
			continue
		}
		generations[g.data.ID] = g
	}
	bootGenerations.replace(generations)
	return nil
}

// sweepBootGenerations removes expired and invalid generations. Each
// generation is removed independently: a failure to remove one generation is
// reported but never prevents another complete boot from continuing and
// never causes a fallback to current assets.
func sweepBootGenerations() error {
	var errs []error
	for _, g := range bootGenerations.list() {
		g.dataLock.Lock()
		expired := g.data.Invalid || time.Now().After(g.data.ExpiresAt)
		g.dataLock.Unlock()
		if !expired {
			continue
		}
		if err := os.RemoveAll(g.dir()); err != nil {
			wwlog.Warn("failed to remove boot generation %s: %s", g.data.ID, err)
			errs = append(errs, fmt.Errorf("could not remove boot generation %s: %w", g.data.ID, err))
			continue
		}
		bootGenerations.delete(g.data.ID)
		wwlog.Info("removed boot generation %s", g.data.ID)
	}
	return errors.Join(errs...)
}
