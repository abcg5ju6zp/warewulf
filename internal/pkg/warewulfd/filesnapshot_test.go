package warewulfd

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/overlay"
	"github.com/warewulf/warewulf/internal/pkg/testenv"
)

const snapshotTestNodesConf = `
nodes:
  n1:
    comment: generation-one
    network devices:
      default:
        hwaddr: 00:00:00:00:00:01
  n2:
    network devices:
      default:
        hwaddr: 00:00:00:00:00:02
`

const snapshotTestNodesConfGen2 = `
nodes:
  n1:
    comment: generation-two
    asset key: newkey
    network devices:
      default:
        hwaddr: 00:00:00:00:00:01
  n2:
    network devices:
      default:
        hwaddr: 00:00:00:00:00:02
  n3:
    network devices:
      default:
        hwaddr: 00:00:00:00:00:03
`

const snapshotTestTemplate = `{{ range .AllNodes }}{{ .Id }},{{ end }}|id={{ .Id }}|comment={{ .Comment }}`

// renderSnapshot renders the staged template of s using the snapshot's own
// node generation. It mirrors the handler's rendering steps.
func renderSnapshot(t *testing.T, s *filesSnapshot) string {
	t.Helper()
	targetNode, err := s.lookupNode(testHwaddr)
	require.NoError(t, err)
	allNodes, err := s.allNodes()
	require.NoError(t, err)
	tstruct, err := overlay.InitStruct("", targetNode, allNodes)
	require.NoError(t, err)
	tstruct.BuildSource = s.templatePath
	buffer, _, _, err := overlay.RenderTemplateFile(s.stagedTemplatePath, tstruct)
	require.NoError(t, err)
	return buffer.String()
}

// A request keeps using its pinned node configuration and template bytes
// even after nodes.conf and the template file are replaced on disk while the
// request is in flight.
func Test_FileSnapshot_PinnedGeneration(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	conf := warewulfconf.Get()
	conf.Paths.WWFilesdir = env.GetPath(testenv.WWFilesdir)
	conf.Warewulf.SecureP = boolPtr(false)

	templatePath := env.GetPath(testenv.WWFilesdir + "/snap.ww")
	env.WriteFile("etc/warewulf/nodes.conf", snapshotTestNodesConf)
	env.WriteFile(testenv.WWFilesdir+"/snap.ww", snapshotTestTemplate)

	snap, err := newFilesSnapshot()
	require.NoError(t, err)
	require.NotEmpty(t, snap.id)
	require.NoError(t, snap.loadNodesConf())
	require.NoError(t, snap.stageTemplate(templatePath))

	// The generation is materialized in the private snapshot directory.
	stagedNodesConf := filepath.Join(snap.dir, "nodes.conf")
	stagedTemplate := filepath.Join(snap.dir, filepath.Base(templatePath))
	assert.FileExists(t, stagedNodesConf)
	assert.FileExists(t, stagedTemplate)

	first := renderSnapshot(t, snap)
	assert.Equal(t, "n1,n2,|id=n1|comment=generation-one", first)

	// Publish a new configuration generation and a new template.
	env.WriteFile("etc/warewulf/nodes.conf", snapshotTestNodesConfGen2)
	env.WriteFile(testenv.WWFilesdir+"/snap.ww", `GENERATION-TWO {{ .Comment }}`)

	// The in-flight snapshot still renders the old configuration against
	// the old template bytes; it never mixes generations.
	again := renderSnapshot(t, snap)
	assert.Equal(t, first, again)
	assert.NotContains(t, again, "n3")
	assert.NotContains(t, again, "generation-two")

	stagedNodesConfData, err := os.ReadFile(stagedNodesConf)
	require.NoError(t, err)
	assert.Contains(t, string(stagedNodesConfData), "generation-one")
	stagedTemplateData, err := os.ReadFile(stagedTemplate)
	require.NoError(t, err)
	assert.Equal(t, snapshotTestTemplate, string(stagedTemplateData))

	snap.remove()
	_, statErr := os.Stat(snap.dir)
	assert.True(t, os.IsNotExist(statErr))
}

// A request that starts after publication uses the new generation.
func Test_HandleFiles_Render_PublicationAffectsLaterRequests(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	env.WriteFile(testenv.WWFilesdir+"/comment.ww", `comment={{ .Comment }}`)
	env.WriteFile("etc/warewulf/nodes.conf", snapshotTestNodesConf)
	assert.NoError(t, LoadNodeDB())

	conf := warewulfconf.Get()
	conf.Paths.WWFilesdir = env.GetPath(testenv.WWFilesdir)
	conf.Warewulf.SecureP = boolPtr(false)

	doRequest := func(assetkey string) string {
		url := "/files/comment.ww?render&wwid=" + testHwaddr
		if assetkey != "" {
			url += "&assetkey=" + assetkey
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		HandleFiles(w, req)
		res := w.Result()
		defer func() { _ = res.Body.Close() }()
		require.Equal(t, http.StatusOK, res.StatusCode)
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return string(data)
	}

	assert.Equal(t, "comment=generation-one", doRequest(""))

	env.WriteFile("etc/warewulf/nodes.conf", snapshotTestNodesConfGen2)
	// The new generation changes both a rendered field and the asset key:
	// authentication and output both follow the newly published config.
	assert.Equal(t, "comment=generation-two", doRequest("newkey"))

	// The old generation's (absent) asset key is no longer accepted.
	req := httptest.NewRequest(http.MethodGet, "/files/comment.ww?render&wwid="+testHwaddr, nil)
	w := httptest.NewRecorder()
	HandleFiles(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode)
}

// Missing nodes.conf fails snapshot acquisition distinctly and the handler
// answers 500 rather than falling back to the in-memory NodeDB generation.
func Test_FileSnapshot_LoadNodesConfUnavailable(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	env.WriteFile("etc/warewulf/nodes.conf", testNodesConf)
	snap, err := newFilesSnapshot()
	require.NoError(t, err)
	defer snap.remove()
	require.NoError(t, snap.loadNodesConf())

	// Removing nodes.conf makes the next generation unavailable even though
	// the in-memory NodeDB still knows the node.
	require.NoError(t, os.Remove(warewulfconf.Get().Paths.NodesConf()))
	snap2, err := newFilesSnapshot()
	require.NoError(t, err)
	defer snap2.remove()
	err = snap2.loadNodesConf()
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errNodesConfUnavailable))

	// Malformed YAML is a parse failure, distinct from the read failure.
	require.NoError(t, os.WriteFile(warewulfconf.Get().Paths.NodesConf(), []byte("nodes: ["), 0o600))
	snap3, err := newFilesSnapshot()
	require.NoError(t, err)
	defer snap3.remove()
	err = snap3.loadNodesConf()
	assert.Error(t, err)
	assert.False(t, errors.Is(err, errNodesConfUnavailable))
}

// Rendering while nodes.conf is unavailable returns 500 even though the
// in-memory NodeDB (loaded earlier) could authenticate the node.
func Test_HandleFiles_Render_NodesConfUnavailable(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	env.WriteFile(testenv.WWFilesdir+"/template.ww", "node={{ .Id }}")
	env.WriteFile("etc/warewulf/nodes.conf", testNodesConf)
	assert.NoError(t, LoadNodeDB())

	conf := warewulfconf.Get()
	conf.Paths.WWFilesdir = env.GetPath(testenv.WWFilesdir)
	conf.Warewulf.SecureP = boolPtr(false)

	require.NoError(t, os.Remove(conf.Paths.NodesConf()))

	req := httptest.NewRequest(http.MethodGet, "/files/template.ww?render&wwid="+testHwaddr, nil)
	w := httptest.NewRecorder()
	HandleFiles(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Result().StatusCode)

	entries, _ := os.ReadDir(filesSnapshotRoot())
	assert.Empty(t, entries, "failed request must not leave snapshot directories")
}

// Stable file reads detect an atomic replacement (rename over) and an
// in-place size change while the file is open.
func Test_ReadFileStable_ChangeDetection(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	path := env.GetPath("template.ww")
	replacement := env.GetPath("template.new")

	t.Run("unchanged file is read completely", func(t *testing.T) {
		env.WriteFile("template.ww", "stable content")
		data, err := readFileStable(path)
		require.NoError(t, err)
		assert.Equal(t, "stable content", string(data))
	})

	t.Run("replacement during read", func(t *testing.T) {
		env.WriteFile("template.ww", "original")
		env.WriteFile("template.new", "replaced content")

		f, err := os.Open(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		before, err := f.Stat()
		require.NoError(t, err)
		data, err := io.ReadAll(f)
		require.NoError(t, err)

		// The template is replaced (new inode) while the request reads it.
		require.NoError(t, os.Rename(replacement, path))

		err = verifyFileRead(f, before, path, data)
		assert.ErrorIs(t, err, errFileChangedDuringRead)
	})

	t.Run("in-place size change during read", func(t *testing.T) {
		env.WriteFile("template.ww", "original")

		f, err := os.Open(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		before, err := f.Stat()
		require.NoError(t, err)
		data, err := io.ReadAll(f)
		require.NoError(t, err)

		out, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		require.NoError(t, err)
		_, err = out.WriteString("appended")
		require.NoError(t, err)
		require.NoError(t, out.Close())

		err = verifyFileRead(f, before, path, data)
		assert.ErrorIs(t, err, errFileChangedDuringRead)
	})

	t.Run("disappearance during read", func(t *testing.T) {
		env.WriteFile("template.ww", "original")

		f, err := os.Open(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		before, err := f.Stat()
		require.NoError(t, err)
		data, err := io.ReadAll(f)
		require.NoError(t, err)

		require.NoError(t, os.Remove(path))

		err = verifyFileRead(f, before, path, data)
		assert.ErrorIs(t, err, errFileChangedDuringRead)
	})
}

// Startup cleanup removes snapshot directories left by a previous process
// and the root is lazily recreated for new requests.
func Test_CleanupFileSnapshots(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	root := filesSnapshotRoot()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "request-stale-1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "request-stale-1", "nodes.conf"), []byte("{}"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "request-stale-2"), 0o700))

	require.NoError(t, CleanupFileSnapshots())
	_, err := os.Stat(root)
	assert.True(t, os.IsNotExist(err))

	snap, err := newFilesSnapshot()
	require.NoError(t, err)
	snap.remove()
}

// failingResponseWriter behaves like a client that accepts the response
// headers but then disconnects before receiving the body.
type failingResponseWriter struct {
	header     http.Header
	status     int
	writeCalls int
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *failingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *failingResponseWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	return 0, errors.New("broken pipe")
}

// A client that disappears while receiving the rendered body must not cause
// a panic, a fallback response from another generation or a leftover
// snapshot directory.
func Test_HandleFiles_Render_ClientDisconnect(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	env.WriteFile(testenv.WWFilesdir+"/template.ww", "node={{ .Id }}")
	env.WriteFile("etc/warewulf/nodes.conf", testNodesConf)
	assert.NoError(t, LoadNodeDB())

	conf := warewulfconf.Get()
	conf.Paths.WWFilesdir = env.GetPath(testenv.WWFilesdir)
	conf.Warewulf.SecureP = boolPtr(false)

	req := httptest.NewRequest(http.MethodGet, "/files/template.ww?render&wwid="+testHwaddr, nil)
	w := &failingResponseWriter{}
	assert.NotPanics(t, func() {
		HandleFiles(w, req)
	})
	assert.Greater(t, w.writeCalls, 0)

	entries, _ := os.ReadDir(filesSnapshotRoot())
	assert.Empty(t, entries, "interrupted request must not leave snapshot directories")
}

// A successful render removes its snapshot directory when done.
func Test_HandleFiles_Render_SnapshotCleanup(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()

	env.WriteFile(testenv.WWFilesdir+"/template.ww", "node={{ .Id }}")
	env.WriteFile("etc/warewulf/nodes.conf", testNodesConf)

	conf := warewulfconf.Get()
	conf.Paths.WWFilesdir = env.GetPath(testenv.WWFilesdir)
	conf.Warewulf.SecureP = boolPtr(false)

	req := httptest.NewRequest(http.MethodGet, "/files/template.ww?render&wwid="+testHwaddr, nil)
	w := httptest.NewRecorder()
	HandleFiles(w, req)
	res := w.Result()
	defer func() { _ = res.Body.Close() }()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	if _, err := os.Stat(filesSnapshotRoot()); err == nil {
		entries, readErr := os.ReadDir(filesSnapshotRoot())
		require.NoError(t, readErr)
		assert.Empty(t, entries)
	}
}
