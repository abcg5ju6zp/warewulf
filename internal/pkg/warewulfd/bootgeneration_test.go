package warewulfd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/testenv"
)

const bootGenerationNodesConf = `nodeprofiles:
  default:
    image name: suse
nodes:
  n1:
    profiles:
    - default
    network devices:
      default:
        hwaddr: 00:00:00:ff:ff:ff
`

const bootGenerationHwaddr = "00:00:00:ff:ff:ff"

var bootIDPattern = regexp.MustCompile(`wwboot=(wwboot-[0-9]{8}-[0-9]{6}-[0-9a-f]{6})`)

// setupBootGenerationEnv builds a complete bootable environment: kernel and
// initramfs in the chroot, a provision image and overlays with gz variants,
// all with distinct "old" content markers.
func setupBootGenerationEnv(t *testing.T) *testenv.TestEnv {
	t.Helper()
	env := testenv.New(t)
	env.WriteFile("/etc/warewulf/nodes.conf", bootGenerationNodesConf)
	env.ImportFile("etc/warewulf/ipxe/default.ipxe", "../../../etc/ipxe/default.ipxe")

	env.CreateFile("/var/lib/warewulf/chroots/suse/rootfs/boot/vmlinuz-1.1.0")
	env.CreateFile("/var/lib/warewulf/chroots/suse/rootfs/boot/initramfs-1.1.0.img")

	env.WriteFile("/srv/warewulf/images/suse.img", "old image")
	env.WriteFile("/srv/warewulf/images/suse.img.gz", "old image gz")

	env.WriteFile("/srv/warewulf/overlays/n1/__SYSTEM__.img", "old system")
	env.WriteFile("/srv/warewulf/overlays/n1/__SYSTEM__.img.gz", "old system gz")
	env.WriteFile("/srv/warewulf/overlays/n1/__RUNTIME__.img", "old runtime")
	env.WriteFile("/srv/warewulf/overlays/n1/__RUNTIME__.img.gz", "old runtime gz")

	require.NoError(t, LoadNodeDB())

	conf := warewulfconf.Get()
	secureFalse := false
	conf.Warewulf.SecureP = &secureFalse
	conf.Ipaddr = "10.10.0.1"
	return env
}

func bootRequest(t *testing.T, url string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = "10.10.10.10:9873"
	w := httptest.NewRecorder()
	HandleProvision(w, req)
	res := w.Result()
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, string(data)
}

// startPinnedBoot requests the iPXE descriptor and returns the pinned
// generation id embedded in it.
func startPinnedBoot(t *testing.T) string {
	t.Helper()
	status, body := bootRequest(t, "/ipxe/"+bootGenerationHwaddr)
	require.Equal(t, http.StatusOK, status, body)
	match := bootIDPattern.FindStringSubmatch(body)
	require.NotNil(t, match, "descriptor must embed the boot generation: %s", body)
	return match[1]
}

// atomicWrite replaces a file the way the provision builders do: temp file
// in the same directory, then rename.
func atomicWrite(t *testing.T, name, content string) {
	t.Helper()
	dir := path.Dir(name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	tmp, err := os.CreateTemp(dir, ".test-*")
	require.NoError(t, err)
	_, err = tmp.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	require.NoError(t, os.Rename(tmp.Name(), name))
}

// createCompleteImageAssets materializes the non-overlay boot assets for
// imageName with kernel version kver: chroot kernel and initramfs files and
// the provision image with gz variant. Node overlay images are built on
// demand by the descriptor when autobuild is enabled.
func createCompleteImageAssets(t *testing.T, env *testenv.TestEnv, imageName, kver string) {
	t.Helper()
	rootfs := "/var/lib/warewulf/chroots/" + imageName + "/rootfs"
	env.CreateFile(rootfs + "/boot/vmlinuz-" + kver)
	env.CreateFile(rootfs + "/boot/initramfs-" + kver + ".img")
	env.CreateFile("/srv/warewulf/images/" + imageName + ".img")
	env.CreateFile("/srv/warewulf/images/" + imageName + ".img.gz")
}

// Test: a descriptor for an incomplete asset set serves an explicit retry
// script rather than rendering asset URLs.
func Test_BootGenerationDescriptorRetry(t *testing.T) {
	env := testenv.New(t)
	defer env.RemoveAll()
	// Node configured with an image, but no boot assets exist on disk.
	env.WriteFile("/etc/warewulf/nodes.conf", `nodes:
  n1:
    image name: suse
    network devices:
      default:
        hwaddr: 00:00:00:ff:ff:ff
`)
	require.NoError(t, LoadNodeDB())
	conf := warewulfconf.Get()
	secureFalse := false
	conf.Warewulf.SecureP = &secureFalse

	status, body := bootRequest(t, "/ipxe/"+bootGenerationHwaddr)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "Boot assets are incomplete")
	assert.Contains(t, body, "reboot")

	status, body = bootRequest(t, "/grub/"+bootGenerationHwaddr)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "Boot assets are incomplete")
}

// Test: every stage of a pinned boot resolves to the generation's files.
func Test_BootGenerationPinsStages(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	id := startPinnedBoot(t)

	cases := []struct {
		url      string
		expected string
	}{
		{"/kernel/" + bootGenerationHwaddr + "?wwboot=" + id, ""},
		{"/initramfs/" + bootGenerationHwaddr + "?wwboot=" + id, ""},
		{"/image/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz", "old image gz"},
		{"/system/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz", "old system gz"},
		{"/runtime/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz", "old runtime gz"},
		{"/image/" + bootGenerationHwaddr + "?wwboot=" + id, "old image"},
	}
	for _, c := range cases {
		status, body := bootRequest(t, c.url)
		assert.Equal(t, http.StatusOK, status, c.url)
		if c.expected != "" {
			assert.Equal(t, c.expected, body, c.url)
		}
	}
}

// Test: a nodes.conf reload and an asset rebuild after the descriptor cannot
// change an in-progress boot; the new assets serve only boots started later.
func Test_BootGenerationSurvivesReloadAndRebuild(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	id := startPinnedBoot(t)
	conf := warewulfconf.Get()

	// Rebuild assets atomically, as wwctl would.
	atomicWrite(t, path.Join(conf.Paths.WWProvisiondir, "images/suse.img.gz"), "new image gz")
	atomicWrite(t, path.Join(conf.Paths.OverlayProvisiondir(), "n1/__SYSTEM__.img.gz"), "new system gz")
	atomicWrite(t, path.Join(conf.Paths.OverlayProvisiondir(), "n1/__RUNTIME__.img.gz"), "new runtime gz")

	// Reload nodes.conf with a content change (node still configured).
	env.WriteFile("/etc/warewulf/nodes.conf", `nodeprofiles:
  default:
    image name: suse
    comment: changed mid-boot
nodes:
  n1:
    profiles:
    - default
    network devices:
      default:
        hwaddr: 00:00:00:ff:ff:ff
`)
	require.NoError(t, LoadNodeDB())

	// The pinned boot keeps serving its complete old files.
	cases := []struct {
		url      string
		expected string
	}{
		{"/image/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz", "old image gz"},
		{"/system/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz", "old system gz"},
		{"/runtime/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz", "old runtime gz"},
	}
	for _, c := range cases {
		status, body := bootRequest(t, c.url)
		assert.Equal(t, http.StatusOK, status, c.url)
		assert.Equal(t, c.expected, body, c.url)
	}

	// A boot started afterwards resolves against the current new assets.
	status, body := bootRequest(t, "/image/"+bootGenerationHwaddr+"?compress=gz")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "new image gz", body)

	// Removing the node and reloading still does not disturb the pinned boot.
	env.WriteFile("/etc/warewulf/nodes.conf", "nodes: {}\n")
	require.NoError(t, LoadNodeDB())
	status, body = bootRequest(t, "/system/"+bootGenerationHwaddr+"?wwboot="+id+"&compress=gz")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "old system gz", body)
}

// Test: a missing pinned file invalidates the whole generation: every later
// request of the boot gets 410, never a replacement file.
func Test_BootGenerationMissingFile(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	id := startPinnedBoot(t)

	g, ok := bootGenerations.get(id)
	require.True(t, ok)
	runtimeGz := g.data.Assets["runtime"].GzProtected
	require.NoError(t, os.Remove(runtimeGz))

	// The request that finds the gap and subsequent requests all 410.
	for _, url := range []string{
		"/runtime/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz",
		"/image/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz",
		"/kernel/" + bootGenerationHwaddr + "?wwboot=" + id,
	} {
		status, body := bootRequest(t, url)
		assert.Equal(t, http.StatusGone, status, url)
		assert.NotContains(t, body, "old ", url)
	}
}

// Test: unknown or malformed generations never fall back to current files.
func Test_BootGenerationUnknown(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	startPinnedBoot(t)

	cases := []string{
		"/image/" + bootGenerationHwaddr + "?wwboot=wwboot-20000101-000000-abcdef&compress=gz",
		"/image/" + bootGenerationHwaddr + "?wwboot=bogus&compress=gz",
	}
	for _, url := range cases {
		status, body := bootRequest(t, url)
		assert.Equal(t, http.StatusGone, status, url)
		assert.NotContains(t, body, "old ", url)
	}
}

// Test: a generation is bound to exactly one node; a different hwaddr cannot
// use it even with the correct generation id.
func Test_BootGenerationIdentityBound(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	id := startPinnedBoot(t)

	status, _ := bootRequest(t, "/image/00:00:00:aa:aa:aa?wwboot="+id+"&compress=gz")
	assert.Equal(t, http.StatusGone, status)
}

// Test: after a daemon restart, an intact generation is loaded from disk and
// the boot continues with the same files.
func Test_BootGenerationRestartPersistence(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	id := startPinnedBoot(t)

	require.NoError(t, LoadBootGenerations()) // simulate process restart

	status, body := bootRequest(t, "/system/"+bootGenerationHwaddr+"?wwboot="+id+"&compress=gz")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "old system gz", body)
}

// Test: a generation whose protected files disappeared before restart is not
// loaded and directs the node to restart.
func Test_BootGenerationBrokenAfterRestart(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	id := startPinnedBoot(t)

	g, ok := bootGenerations.get(id)
	require.True(t, ok)
	require.NoError(t, os.Remove(g.data.Assets["system"].Protected))

	require.NoError(t, LoadBootGenerations())

	status, _ := bootRequest(t, "/system/" + bootGenerationHwaddr + "?wwboot=" + id + "&compress=gz")
	assert.Equal(t, http.StatusGone, status)
}

// Test: the sweep removes expired generations only, and a failure to remove
// one generation never prevents another from being removed.
func Test_BootGenerationSweep(t *testing.T) {
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()
	fresh := startPinnedBoot(t)

	// A second, expired generation for the same node.
	registry, err := node.New()
	require.NoError(t, err)
	n1, err := registry.GetNode("n1")
	require.NoError(t, err)
	expiredGen, err := pinBootGeneration(
		parsedRequest{hwaddr: bootGenerationHwaddr},
		n1, false)
	require.NoError(t, err)
	expiredGen.dataLock.Lock()
	expiredGen.data.ExpiresAt = expiredGen.data.CreatedAt
	require.NoError(t, expiredGen.persist())
	expiredGen.dataLock.Unlock()

	require.NoError(t, sweepBootGenerations())
	_, freshOK := bootGenerations.get(fresh)
	assert.True(t, freshOK, "unexpired generation must be retained")
	_, expiredOK := bootGenerations.get(expiredGen.data.ID)
	assert.False(t, expiredOK, "expired generation must be removed")
}

// Test: a cleanup failure is isolated: one unremovable generation does not
// block removal of another expired generation. Skipped when running as root,
// which bypasses directory permissions.
func Test_BootGenerationSweepFailureIsolated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	env := setupBootGenerationEnv(t)
	defer env.RemoveAll()

	registry, err := node.New()
	require.NoError(t, err)
	n1, err := registry.GetNode("n1")
	require.NoError(t, err)

	g, err := pinBootGeneration(
		parsedRequest{hwaddr: bootGenerationHwaddr},
		n1, false)
	require.NoError(t, err)

	// Make its assets directory unreadable so RemoveAll fails; restore it
	// before the test environment is removed.
	assetsDir := path.Join(g.dir(), "assets")
	require.NoError(t, os.Chmod(assetsDir, 0o000))
	defer func() { _ = os.Chmod(assetsDir, 0o750) }()
	g.dataLock.Lock()
	g.data.ExpiresAt = g.data.CreatedAt
	require.NoError(t, g.persist())
	g.dataLock.Unlock()

	// Another expired generation with normal permissions.
	other, err := pinBootGeneration(
		parsedRequest{hwaddr: bootGenerationHwaddr},
		n1, false)
	require.NoError(t, err)
	other.dataLock.Lock()
	other.data.ExpiresAt = other.data.CreatedAt
	require.NoError(t, other.persist())
	other.dataLock.Unlock()

	err = sweepBootGenerations()
	assert.Error(t, err, "sweep reports the failure")
	_, blocked := bootGenerations.get(g.data.ID)
	assert.True(t, blocked, "failed generation remains in place")
	_, removed := bootGenerations.get(other.data.ID)
	assert.False(t, removed, "other expired generation is still removed")
}
