package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPersistToFileAtomic(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "nodes.conf")

	db := mustParse(t, `
nodeprofiles:
  default: {}
nodes: {}
`)
	assert.NoError(t, db.PersistToFile(configFile))

	data, err := os.ReadFile(configFile)
	assert.NoError(t, err)
	assert.Contains(t, string(data), "default")

	// no temporary files are left behind
	entries, err := os.ReadDir(dir)
	assert.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".tmp")
	}

	// the published file round-trips through Parse
	reloaded, err := Parse(data)
	assert.NoError(t, err)
	_, err = reloaded.GetProfile("default")
	assert.NoError(t, err)
}

func TestPersistToFileAtomicFailureKeepsOldVersion(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "nodes.conf")

	old := mustParse(t, dependencyTopology)
	assert.NoError(t, old.PersistToFile(configFile))
	before, err := os.ReadFile(configFile)
	assert.NoError(t, err)

	originHook := atomicWriteConfig
	atomicWriteConfig = func(string, []byte, os.FileMode) error {
		return errors.New("injected publish failure")
	}
	t.Cleanup(func() { atomicWriteConfig = originHook })

	fresh := mustParse(t, `
nodeprofiles:
  changed: {}
nodes: {}
`)
	err = fresh.PersistToFile(configFile)
	assert.Error(t, err)

	// old version remains on disk and stays parseable
	after, err := os.ReadFile(configFile)
	assert.NoError(t, err)
	assert.Equal(t, before, after)
	_, parseErr := Parse(after)
	assert.NoError(t, parseErr)

	// retrying (failure cleared) publishes the new version
	atomicWriteConfig = originHook
	assert.NoError(t, fresh.PersistToFile(configFile))
	published, err := os.ReadFile(configFile)
	assert.NoError(t, err)
	reloaded, err := Parse(published)
	assert.NoError(t, err)
	_, err = reloaded.GetProfile("changed")
	assert.NoError(t, err)
}
