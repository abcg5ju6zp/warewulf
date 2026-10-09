package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func mustParse(t *testing.T, data string) NodesYaml {
	t.Helper()
	db, err := Parse([]byte(data))
	assert.NoError(t, err)
	return db
}

const dependencyTopology = `
nodeprofiles:
  base:
    kernel:
      args:
        - old-arg
  a:
    profiles:
      - base
  b:
    profiles:
      - a
nodes:
  n1:
    profiles:
      - a
  n2:
    profiles:
      - b
  n3: {}
`

func TestProfileReverseClosure(t *testing.T) {
	db := mustParse(t, dependencyTopology)

	assert.Equal(t, []string{"a", "b", "base"}, db.ProfileReverseClosure([]string{"base"}))
	assert.Equal(t, []string{"a", "b"}, db.ProfileReverseClosure([]string{"a"}))
	assert.Equal(t, []string{"b"}, db.ProfileReverseClosure([]string{"b"}))
	// duplicate roots do not change the closure
	assert.Equal(t, []string{"a", "b", "base"}, db.ProfileReverseClosure([]string{"base", "base"}))
}

func TestCheckProfileCycles(t *testing.T) {
	t.Run("acyclic graph", func(t *testing.T) {
		db := mustParse(t, dependencyTopology)
		assert.NoError(t, db.CheckProfileCycles())
	})

	t.Run("self reference", func(t *testing.T) {
		db := mustParse(t, `
nodeprofiles:
  x:
    profiles:
      - x
nodes: {}
`)
		var cycleErr *ProfileCycleError
		err := db.CheckProfileCycles()
		assert.ErrorAs(t, err, &cycleErr)
		assert.Contains(t, cycleErr.Cycle, "x")
	})

	t.Run("two node cycle", func(t *testing.T) {
		db := mustParse(t, `
nodeprofiles:
  x:
    profiles:
      - y
  y:
    profiles:
      - x
nodes: {}
`)
		var cycleErr *ProfileCycleError
		err := db.CheckProfileCycles()
		assert.ErrorAs(t, err, &cycleErr)
		assert.Equal(t, []string{"x", "y", "x"}, cycleErr.Cycle)
		assert.Contains(t, err.Error(), "x -> y -> x")
	})

	t.Run("three node cycle", func(t *testing.T) {
		db := mustParse(t, `
nodeprofiles:
  a:
    profiles:
      - b
  b:
    profiles:
      - c
  c:
    profiles:
      - a
nodes: {}
`)
		var cycleErr *ProfileCycleError
		err := db.CheckProfileCycles()
		assert.ErrorAs(t, err, &cycleErr)
		assert.Len(t, cycleErr.Cycle, 4)
		assert.Equal(t, cycleErr.Cycle[0], cycleErr.Cycle[len(cycleErr.Cycle)-1])
	})

	t.Run("negated reference is not an edge", func(t *testing.T) {
		db := mustParse(t, `
nodeprofiles:
  x:
    profiles:
      - "~y"
  y:
    profiles:
      - x
nodes: {}
`)
		assert.NoError(t, db.CheckProfileCycles())
		// negated reference creates no reverse edge either
		assert.Equal(t, []string{"x", "y"}, db.ProfileReverseClosure([]string{"x"}))
	})

	t.Run("dangling reference does not error", func(t *testing.T) {
		db := mustParse(t, `
nodeprofiles:
  x:
    profiles:
      - does-not-exist
nodes: {}
`)
		assert.NoError(t, db.CheckProfileCycles())
	})
}

func TestAffectedNodes(t *testing.T) {
	prev := mustParse(t, dependencyTopology)
	next := mustParse(t, `
nodeprofiles:
  base:
    kernel:
      args:
        - new-arg
  a:
    profiles:
      - base
  b:
    profiles:
      - a
nodes:
  n1:
    profiles:
      - a
  n2:
    profiles:
      - b
  n3: {}
`)

	t.Run("nested dependents affected, unrelated node excluded", func(t *testing.T) {
		assert.Equal(t, []string{"n1", "n2"}, next.AffectedNodes(&prev, []string{"base"}))
	})

	t.Run("unchanged content never affects nodes, even for referenced roots", func(t *testing.T) {
		assert.Equal(t, []string{}, prev.AffectedNodes(&prev, []string{"b"}))
	})
}

func TestAffectedNodesOverridesAndNegation(t *testing.T) {
	prev := mustParse(t, `
nodeprofiles:
  base:
    image name: baseimg
    kernel:
      args:
        - keep-arg
  a:
    profiles:
      - base
nodes:
  n-override:
    profiles:
      - base
    image name: nodeimg
  n-negated:
    profiles:
      - a
      - "~base"
  n-unrelated: {}
`)
	// base changes its image to the value n-override already pins at the
	// node level; the merged result of n-override is unchanged, as is
	// n-negated (base negated via a nested reference).
	next := mustParse(t, `
nodeprofiles:
  base:
    image name: nodeimg
    kernel:
      args:
        - keep-arg
  a:
    profiles:
      - base
nodes:
  n-override:
    profiles:
      - base
    image name: nodeimg
  n-negated:
    profiles:
      - a
      - "~base"
  n-unrelated: {}
`)
	assert.Equal(t, []string{}, next.AffectedNodes(&prev, []string{"base"}))

	// when base changes a field the node does not pin (kernel args), the
	// overriding node becomes a genuine dependent while the negated and
	// unrelated nodes remain excluded.
	next2 := mustParse(t, `
nodeprofiles:
  base:
    image name: baseimg
    kernel:
      args:
        - new-arg
  a:
    profiles:
      - base
nodes:
  n-override:
    profiles:
      - base
    image name: nodeimg
  n-negated:
    profiles:
      - a
      - "~base"
  n-unrelated: {}
`)
	affected := next2.AffectedNodes(&prev, []string{"base"})
	assert.Contains(t, affected, "n-override")
	assert.NotContains(t, affected, "n-negated")
	assert.NotContains(t, affected, "n-unrelated")
}

func TestAffectedNodesFieldTypes(t *testing.T) {
	prev := mustParse(t, `
nodeprofiles:
  base:
    image name: oldimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
nodes:
  n1:
    profiles:
      - base
`)
	for name, nextYAML := range map[string]string{
		"image": `
nodeprofiles:
  base:
    image name: newimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-a
nodes:
  n1:
    profiles:
      - base
`,
		"system overlay": `
nodeprofiles:
  base:
    image name: oldimg
    system overlay:
      - so-b
    runtime overlay:
      - ro-a
nodes:
  n1:
    profiles:
      - base
`,
		"runtime overlay": `
nodeprofiles:
  base:
    image name: oldimg
    system overlay:
      - so-a
    runtime overlay:
      - ro-b
nodes:
  n1:
    profiles:
      - base
`,
	} {
		t.Run(name, func(t *testing.T) {
			next := mustParse(t, nextYAML)
			assert.Equal(t, []string{"n1"}, next.AffectedNodes(&prev, []string{"base"}))
		})
	}

	// identical content: nothing affected
	assert.Empty(t, prev.AffectedNodes(&prev, []string{"base"}))
}

func TestCloneIsIndependent(t *testing.T) {
	orig := mustParse(t, dependencyTopology)
	clone, err := orig.Clone()
	assert.NoError(t, err)

	base, err := clone.GetProfilePtr("base")
	assert.NoError(t, err)
	base.ImageName = "changed"

	origBase, err := orig.GetProfile("base")
	assert.NoError(t, err)
	assert.NotEqual(t, "changed", origBase.ImageName)

	merged, _, err := clone.MergeNode("n1")
	assert.NoError(t, err)
	assert.True(t, merged.Valid())
}
