package warewulfd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// Per-request configuration and template generations are materialized in a
// private snapshot directory so that every byte of a rendered response
// (authentication data, target node, allNodes, profile field sources,
// template content and rendered output) derives from one generation. The
// files directory served for static /files requests must never be used as
// the snapshot root.
const filesSnapshotDirName = "files-snapshots"

var (
	// errNodesConfUnavailable indicates that nodes.conf could not be read
	// as one stable generation while acquiring a snapshot.
	errNodesConfUnavailable = errors.New("node configuration is unavailable")
	// errFileChangedDuringRead indicates that a file was replaced,
	// truncated, extended or otherwise modified while it was being read
	// into a snapshot. The request must fail rather than continue with a
	// different generation.
	errFileChangedDuringRead = errors.New("file changed during read")
)

// filesSnapshotRoot returns the directory under which per-request snapshot
// directories are created.
func filesSnapshotRoot() string {
	conf := warewulfconf.Get()
	return filepath.Join(conf.Paths.Localstatedir, "warewulf", filesSnapshotDirName)
}

// CleanupFileSnapshots removes snapshot directories left behind by a previous
// daemon process (e.g. requests interrupted by a restart or a crash). It is
// meant to run once at daemon startup; in-flight snapshots of the running
// daemon are removed by the requests themselves and must not be purged from
// a configuration reload.
func CleanupFileSnapshots() error {
	root := filesSnapshotRoot()
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("could not clean leftover file snapshots in %s: %w", root, err)
	}
	wwlog.Debug("removed leftover file snapshots from %s", root)
	return nil
}

// filesSnapshot is one pinned generation of the node configuration and
// template content used by a single /files render request.
type filesSnapshot struct {
	// id is the unique generation identifier (the snapshot directory name).
	id string
	// dir is the private directory holding this generation's bytes.
	dir string
	// yml is the parsed node configuration belonging to this generation.
	yml node.NodesYaml
	// nodeInfo maps hardware addresses to node ids within yml. Discoverable
	// nodes are excluded, matching the in-memory NodeDB semantics.
	nodeInfo map[string]string
	// templatePath is the originally requested template path; it is what
	// the template reports as its build source.
	templatePath string
	// stagedTemplatePath is the immutable copy of templatePath inside dir
	// that is actually rendered.
	stagedTemplatePath string
}

// newFilesSnapshot creates a new, empty generation in a private directory.
// The caller must call remove when the request is finished.
func newFilesSnapshot() (*filesSnapshot, error) {
	root := filesSnapshotRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("could not create snapshot directory %s: %w", root, err)
	}
	dir, err := os.MkdirTemp(root, "request-*")
	if err != nil {
		return nil, fmt.Errorf("could not create request snapshot in %s: %w", root, err)
	}
	return &filesSnapshot{id: filepath.Base(dir), dir: dir}, nil
}

// remove deletes the snapshot directory and its contents. It is safe to call
// repeatedly and on a nil receiver.
func (s *filesSnapshot) remove() {
	if s == nil || s.dir == "" {
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		wwlog.Warn("could not remove file snapshot %s: %s", s.id, err)
	}
}

// loadNodesConf reads nodes.conf as one stable generation, stages the exact
// bytes read in the snapshot directory and parses them. A read failure, a
// file replaced during the read or a parse failure fail snapshot acquisition
// so the request can be rejected without falling back to another generation.
func (s *filesSnapshot) loadNodesConf() error {
	nodesConf := warewulfconf.Get().Paths.NodesConf()
	data, err := readFileStable(nodesConf)
	if err != nil {
		// Any failure to obtain one stable generation (missing file,
		// permission denied, replaced during the read, torn read) is
		// reported as the configuration being unavailable; the request
		// must not fall back to the in-memory NodeDB or any other
		// generation.
		return fmt.Errorf("reading %s failed: %w: %w", nodesConf, errNodesConfUnavailable, err)
	}

	if err := os.WriteFile(filepath.Join(s.dir, "nodes.conf"), data, 0o600); err != nil {
		return fmt.Errorf("could not stage node configuration for snapshot: %w", err)
	}

	yml, err := node.Parse(data)
	if err != nil {
		return fmt.Errorf("could not parse node configuration %s: %w", nodesConf, err)
	}
	s.yml = yml
	s.nodeInfo = snapshotNodeInfo(&yml)
	return nil
}

// snapshotNodeInfo builds the hwaddr-to-node-id index for one parsed node
// configuration generation. Discoverable nodes are skipped, matching
// loadNodeDB, so /files never authenticates a node through discovery.
func snapshotNodeInfo(yml *node.NodesYaml) map[string]string {
	info := make(map[string]string)
	for id, n := range yml.Nodes {
		if n.Discoverable.Bool() {
			continue
		}
		for _, netdev := range n.NetDevs {
			if hwaddr := strings.ToLower(netdev.Hwaddr); hwaddr != "" {
				info[hwaddr] = id
			}
		}
	}
	return info
}

// lookupNode resolves the profile-merged target node for hwaddr within this
// generation.
func (s *filesSnapshot) lookupNode(hwaddr string) (node.Node, error) {
	id, ok := s.nodeInfo[hwaddr]
	if !ok {
		return node.Node{}, fmt.Errorf("no node configured for hwaddr %s", hwaddr)
	}
	return s.yml.GetNode(id)
}

// allNodes returns every profile-merged node of this generation.
func (s *filesSnapshot) allNodes() ([]node.Node, error) {
	return s.yml.FindAllNodes()
}

// stageTemplate reads srcPath as one stable generation and writes the exact
// bytes read to the snapshot directory, from which it is rendered. If the
// template is replaced or modified during the read, acquisition fails with
// errFileChangedDuringRead and no other generation is substituted.
func (s *filesSnapshot) stageTemplate(srcPath string) error {
	data, err := readFileStable(srcPath)
	if err != nil {
		return err
	}

	dest := filepath.Join(s.dir, filepath.Base(srcPath))
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return fmt.Errorf("could not stage template %s for snapshot: %w", srcPath, err)
	}
	s.templatePath = srcPath
	s.stagedTemplatePath = dest
	return nil
}

// readFileStable reads a file in one generation. Before and after the read
// it verifies that the path still identifies the same file with the same
// size and modification time and that all of the file's bytes were read.
// A mismatch means the file changed while it was being read.
func readFileStable(path string) (data []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	before, err := f.Stat()
	if err != nil {
		return nil, err
	}

	data, err = io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	if err = verifyFileRead(f, before, path, data); err != nil {
		return nil, err
	}
	return data, nil
}

// verifyFileRead checks that the file read through f still matches the
// attributes recorded before the read and that the path still refers to the
// same file. It is split out from readFileStable so the replacement-during-
// read condition can be tested deterministically.
func verifyFileRead(f *os.File, before os.FileInfo, path string, data []byte) error {
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		wwlog.Error("file %s changed while being read (size %d -> %d)", path, before.Size(), after.Size())
		return errFileChangedDuringRead
	}

	// The path may have been replaced (atomically renamed over or re-pointed
	// through a symlink) even though the originally opened descriptor is
	// unchanged.
	pathInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			wwlog.Error("file %s disappeared while being read", path)
			return errFileChangedDuringRead
		}
		return err
	}
	if !os.SameFile(before, pathInfo) ||
		pathInfo.Size() != before.Size() ||
		!pathInfo.ModTime().Equal(before.ModTime()) {
		wwlog.Error("file %s was replaced while being read", path)
		return errFileChangedDuringRead
	}

	if int64(len(data)) != before.Size() {
		wwlog.Error("file %s was only partially read (%d of %d bytes)", path, len(data), before.Size())
		return errFileChangedDuringRead
	}
	return nil
}
