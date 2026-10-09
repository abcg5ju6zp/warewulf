package set

import (
	"bytes"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/warewulf/warewulf/internal/pkg/testenv"
	"github.com/warewulf/warewulf/internal/pkg/warewulfd"
)

func profileChangeCLIEnv(t *testing.T, nodesConf string) *testenv.TestEnv {
	t.Helper()
	warewulfd.SetNoDaemon()
	env := testenv.New(t)
	env.MkdirAll("var/lib/warewulf/chroots/testimg/rootfs")
	env.WriteFile("var/lib/warewulf/overlays/so-a/rootfs/etc/hello", "system")
	env.WriteFile("var/lib/warewulf/overlays/ro-a/rootfs/run.txt", "runtime")
	env.WriteFile("etc/warewulf/nodes.conf", nodesConf)
	return env
}

func runProfileSetCmd(t *testing.T, args ...string) error {
	t.Helper()
	baseCmd := GetCommand()
	baseCmd.SetArgs(args)
	buf := new(bytes.Buffer)
	baseCmd.SetOut(buf)
	baseCmd.SetErr(buf)
	return baseCmd.Execute()
}

const cliChangeNodesConf = `
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
`

func TestProfileSetCoordinatesNestedChange(t *testing.T) {
	env := profileChangeCLIEnv(t, cliChangeNodesConf)
	defer env.RemoveAll()

	err := runProfileSetCmd(t, "--kernelargs=new-arg", "--yes", "base")
	assert.NoError(t, err)

	// node inheriting base through profile "a" is rebuilt on both contexts
	assert.FileExists(t, env.GetPath("srv/warewulf/overlays/n1/__SYSTEM__.img"))
	assert.FileExists(t, env.GetPath("srv/warewulf/overlays/n1/__RUNTIME__.img"))
	// converged transaction prunes the journal
	_, err = os.Stat(env.GetPath("srv/warewulf/profile-change.json"))
	assert.True(t, os.IsNotExist(err))
	assert.Contains(t, env.ReadFile("etc/warewulf/nodes.conf"), "new-arg")
}

func TestProfileSetRejectsIntroducedCycle(t *testing.T) {
	env := profileChangeCLIEnv(t, cliChangeNodesConf)
	defer env.RemoveAll()

	before := env.ReadFile("etc/warewulf/nodes.conf")
	err := runProfileSetCmd(t, "--profile=a", "--yes", "base")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
	assert.Equal(t, before, env.ReadFile("etc/warewulf/nodes.conf"))
	_, err = os.Stat(env.GetPath(path.Join("srv/warewulf", "profile-change.json")))
	assert.True(t, os.IsNotExist(err))
}

func TestProfileSetRejectsIntroducedMissingOverlay(t *testing.T) {
	env := profileChangeCLIEnv(t, cliChangeNodesConf)
	defer env.RemoveAll()

	before := env.ReadFile("etc/warewulf/nodes.conf")
	err := runProfileSetCmd(t, "--system-overlays=ghost", "--yes", "base")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")
	assert.Equal(t, before, env.ReadFile("etc/warewulf/nodes.conf"))
	_, err = os.Stat(env.GetPath(path.Join("srv/warewulf", "profile-change.json")))
	assert.True(t, os.IsNotExist(err))
}
