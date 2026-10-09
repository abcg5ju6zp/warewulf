package util_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/warewulf/warewulf/internal/pkg/util"
)

func TestAtomicWriteFileReplacesContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nodes.conf")

	assert.NoError(t, util.AtomicWriteFile(target, []byte("version-1"), 0o644))
	assert.NoError(t, util.AtomicWriteFile(target, []byte("version-2"), 0o644))

	data, err := os.ReadFile(target)
	assert.NoError(t, err)
	assert.Equal(t, "version-2", string(data))

	info, err := os.Stat(target)
	assert.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	assert.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".tmp")
	}
}

func TestAtomicWriteFileFailureLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nodes.conf")
	assert.NoError(t, util.AtomicWriteFile(target, []byte("original"), 0o644))

	// writing to a path whose parent is an existing file fails before the
	// rename and must leave the original file intact.
	err := util.AtomicWriteFile(filepath.Join(target, "sub"), []byte("x"), 0o644)
	assert.Error(t, err)

	data, err := os.ReadFile(target)
	assert.NoError(t, err)
	assert.Equal(t, "original", string(data))
}
