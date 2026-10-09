package api

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/warewulf/warewulf/internal/pkg/testenv"
	"github.com/warewulf/warewulf/internal/pkg/warewulfd"
)

func profileChangeAPITestEnv(t *testing.T, nodesConf string) *testenv.TestEnv {
	t.Helper()
	warewulfd.SetNoDaemon()
	env := testenv.New(t)
	env.MkdirAll("var/lib/warewulf/chroots/testimg/rootfs")
	env.WriteFile("var/lib/warewulf/overlays/so-a/rootfs/etc/hello", "system")
	env.WriteFile("var/lib/warewulf/overlays/ro-a/rootfs/run.txt", "runtime")
	env.WriteFile("/etc/warewulf/nodes.conf", nodesConf)
	return env
}

func doProfileRequest(t *testing.T, srv *httptest.Server, method, urlPath, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+urlPath, bytes.NewBufferString(body))
	assert.NoError(t, err)
	resp, err := http.DefaultTransport.RoundTrip(req)
	assert.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	return resp.StatusCode, string(data)
}

func journalPath(env *testenv.TestEnv) string {
	return env.GetPath(path.Join("srv/warewulf", "profile-change.json"))
}

func nodeOverlayImage(env *testenv.TestEnv, nodeID, context string) string {
	return env.GetPath(path.Join("srv/warewulf/overlays", nodeID, "__"+strings.ToUpper(context)+"__.img"))
}

// preseedOverlayImage writes a sentinel image with an old mtime so the
// freshness check treats nodes.conf as newer.
func preseedOverlayImage(t *testing.T, env *testenv.TestEnv, nodeID, context string) {
	t.Helper()
	imagePath := nodeOverlayImage(env, nodeID, context)
	assert.NoError(t, os.MkdirAll(path.Dir(imagePath), 0o755))
	assert.NoError(t, os.WriteFile(imagePath, []byte("old-generation"), 0o644))
	assert.NoError(t, os.Chtimes(imagePath, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
}

const profileChangeNodesConf = `
nodeprofiles:
  base:
    image name: testimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
    kernel:
      args:
        - old-arg
  a:
    profiles:
      - base
nodes:
  n1:
    profiles:
      - a
  n2:
    profiles:
      - base
  n3: {}
`

func TestProfileUpdateRebuildsAffectedNodes(t *testing.T) {
	env := profileChangeAPITestEnv(t, profileChangeNodesConf)
	defer env.RemoveAll()

	// n2 already carries an older generation image; n3 is unrelated and
	// must keep its (sentinel) images untouched.
	preseedOverlayImage(t, env, "n2", "system")
	preseedOverlayImage(t, env, "n2", "runtime")
	preseedOverlayImage(t, env, "n3", "system")
	preseedOverlayImage(t, env, "n3", "runtime")
	n3SystemUnrelated, err := os.ReadFile(nodeOverlayImage(env, "n3", "system"))
	assert.NoError(t, err)
	n3Stat, err := os.Stat(nodeOverlayImage(env, "n3", "system"))
	assert.NoError(t, err)

	allowedNets := []net.IPNet{{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}}
	srv := httptest.NewServer(Handler(nil, allowedNets))
	defer srv.Close()

	statusCode, body := doProfileRequest(t, srv, http.MethodPatch, "/api/profiles/base",
		`{"profile": {"kernel": {"args": ["new-arg"]}}}`)
	assert.Equal(t, http.StatusOK, statusCode, body)
	assert.Contains(t, body, "new-arg")

	// affected nested (n1) and direct (n2) nodes rebuilt on both contexts
	assert.FileExists(t, nodeOverlayImage(env, "n1", "system"))
	assert.FileExists(t, nodeOverlayImage(env, "n1", "runtime"))
	n2System, err := os.Stat(nodeOverlayImage(env, "n2", "system"))
	assert.NoError(t, err)
	assert.True(t, n2System.ModTime().After(n3Stat.ModTime().Add(-time.Minute)))
	rebuiltN2, err := os.ReadFile(nodeOverlayImage(env, "n2", "system"))
	assert.NoError(t, err)
	assert.NotEqual(t, "old-generation", string(rebuiltN2))

	// unrelated node's image is byte-identical and keeps its mtime
	n3SystemAfter, err := os.ReadFile(nodeOverlayImage(env, "n3", "system"))
	assert.NoError(t, err)
	assert.Equal(t, n3SystemUnrelated, n3SystemAfter)
	n3StatAfter, err := os.Stat(nodeOverlayImage(env, "n3", "system"))
	assert.NoError(t, err)
	assert.Equal(t, n3Stat.ModTime(), n3StatAfter.ModTime())

	// the converged transaction left no journal
	_, err = os.Stat(journalPath(env))
	assert.True(t, os.IsNotExist(err))

	// nodes.conf carries the new kernel argument
	assert.Contains(t, env.ReadFile("/etc/warewulf/nodes.conf"), "new-arg")
}

func TestProfileUpdateRejectsCycleAndMissingReferences(t *testing.T) {
	env := profileChangeAPITestEnv(t, profileChangeNodesConf)
	defer env.RemoveAll()

	allowedNets := []net.IPNet{{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}}
	srv := httptest.NewServer(Handler(nil, allowedNets))
	defer srv.Close()

	beforeConf := env.ReadFile("/etc/warewulf/nodes.conf")

	statusCode, body := doProfileRequest(t, srv, http.MethodPatch, "/api/profiles/base",
		`{"profile": {"profiles": ["a"]}}`)
	assert.Equal(t, http.StatusBadRequest, statusCode, body)
	assert.Contains(t, body, "INVALID_ARGUMENT")
	assert.Equal(t, beforeConf, env.ReadFile("/etc/warewulf/nodes.conf"))
	_, err := os.Stat(journalPath(env))
	assert.True(t, os.IsNotExist(err))

	statusCode, body = doProfileRequest(t, srv, http.MethodPatch, "/api/profiles/base",
		`{"profile": {"system overlay": ["ghost"]}}`)
	assert.Equal(t, http.StatusBadRequest, statusCode, body)
	assert.Contains(t, body, "ghost")
	assert.Equal(t, beforeConf, env.ReadFile("/etc/warewulf/nodes.conf"))
}

func TestProfileQueriesLeaveNoJournal(t *testing.T) {
	env := profileChangeAPITestEnv(t, profileChangeNodesConf)
	defer env.RemoveAll()

	allowedNets := []net.IPNet{{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}}
	srv := httptest.NewServer(Handler(nil, allowedNets))
	defer srv.Close()

	statusCode, _ := doProfileRequest(t, srv, http.MethodGet, "/api/profiles", "")
	assert.Equal(t, http.StatusOK, statusCode)
	statusCode, _ = doProfileRequest(t, srv, http.MethodGet, "/api/profiles/base", "")
	assert.Equal(t, http.StatusOK, statusCode)

	_, err := os.Stat(journalPath(env))
	assert.True(t, os.IsNotExist(err))
}
