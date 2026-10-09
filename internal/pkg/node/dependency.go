package node

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

// ProfileCycleError describes a cycle in the profile reference graph. The
// Cycle slice lists the profiles forming the cycle, with the first profile
// repeated at the end (e.g. [a, b, a]).
type ProfileCycleError struct {
	Cycle []string
}

func (e *ProfileCycleError) Error() string {
	return fmt.Sprintf("profile cycle detected: %s", strings.Join(e.Cycle, " -> "))
}

// profileRefs returns the non-negated, existing profiles directly referenced
// by the given profile, in deterministic (sorted) order. Negated tokens
// ("~name") and references to profiles that do not exist are not treated as
// graph edges.
func (config *NodesYaml) profileRefs(profileID string) []string {
	profile, ok := config.NodeProfiles[profileID]
	if !ok {
		return nil
	}
	refs := make([]string, 0, len(profile.Profiles))
	for _, ref := range profile.Profiles {
		if strings.HasPrefix(ref, "~") {
			continue
		}
		if _, ok := config.NodeProfiles[ref]; !ok {
			continue
		}
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return refs
}

// CheckProfileCycles verifies that the profile reference graph is acyclic.
// Edges are non-negated references to existing profiles; self references
// count as cycles. A [ProfileCycleError] listing one detected cycle is
// returned, or nil when the graph is acyclic.
func (config *NodesYaml) CheckProfileCycles() error {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int)
	var stack []string

	var visit func(id string) error
	visit = func(id string) error {
		color[id] = gray
		stack = append(stack, id)
		for _, ref := range config.profileRefs(id) {
			switch color[ref] {
			case white:
				if err := visit(ref); err != nil {
					return err
				}
			case gray:
				start := slices.Index(stack, ref)
				cycle := append([]string{}, stack[start:]...)
				cycle = append(cycle, ref)
				return &ProfileCycleError{Cycle: cycle}
			}
		}
		color[id] = black
		stack = stack[:len(stack)-1]
		return nil
	}

	for _, id := range config.ListAllProfiles() {
		if color[id] == white {
			if err := visit(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProfileReverseClosure returns the sorted, deduplicated set of profile IDs
// that transitively reference any of the given roots, together with the
// roots themselves. Negated references ("~name") do not create edges.
func (config *NodesYaml) ProfileReverseClosure(roots []string) []string {
	reverse := make(map[string][]string)
	for id, profile := range config.NodeProfiles {
		for _, ref := range profile.Profiles {
			if strings.HasPrefix(ref, "~") {
				continue
			}
			if _, ok := config.NodeProfiles[ref]; !ok {
				continue
			}
			reverse[ref] = append(reverse[ref], id)
		}
	}
	for ids := range reverse {
		slices.Sort(reverse[ids])
	}

	seen := make(map[string]bool)
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, dependent := range reverse[id] {
			walk(dependent)
		}
	}
	for _, root := range roots {
		walk(root)
	}

	closure := make([]string, 0, len(seen))
	for id := range seen {
		closure = append(closure, id)
	}
	slices.Sort(closure)
	return closure
}

// AffectedNodes returns the sorted IDs of nodes whose fully merged
// configuration changes between prev and the receiver, restricted to nodes
// whose effective (nested and negation-aware) profile list intersects the
// reverse dependency closure of roots.
//
// Nodes that override inherited fields explicitly, that only negate the
// changed profiles, or that are unrelated to roots are excluded: the merged
// node comparison is the final arbiter, because overlay templates may depend
// on any merged field.
func (next *NodesYaml) AffectedNodes(prev *NodesYaml, roots []string) []string {
	closure := next.ProfileReverseClosure(roots)
	inClosure := make(map[string]bool, len(closure))
	for _, id := range closure {
		inClosure[id] = true
	}

	affected := []string{}
	for _, id := range next.ListAllNodes() {
		profiles := next.getNodeProfiles(id)
		candidate := false
		for _, profileID := range profiles {
			if inClosure[profileID] {
				candidate = true
				break
			}
		}
		if !candidate {
			continue
		}

		mergedNext, _, err := next.MergeNode(id)
		if err != nil {
			// Excluding a node whose merged result cannot be computed
			// could silently skip a required overlay rebuild, so surface
			// this rather than dropping it quietly.
			wwlog.Warn("could not merge node %s while computing affected nodes: %v", id, err)
			continue
		}
		mergedPrev, _, err := prev.MergeNode(id)
		if err != nil {
			// The node is present in next but could not be merged from prev:
			// treat it as affected rather than skipping an observable change.
			affected = append(affected, id)
			continue
		}
		if !reflect.DeepEqual(mergedPrev, mergedNext) {
			affected = append(affected, id)
		}
	}
	slices.Sort(affected)
	return affected
}
