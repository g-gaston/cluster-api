package inplaceupgrade

import (
	"encoding/json"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/controlplane/upgrade"
)

func listNotAllowedChanges(spec *upgrade.MachineSpec, actual, allowed *machineSpedChanges) []string {
	var list []string
	if notAllowed := notAllowedChanges(actual.machinePaths, allowed.machinePaths); notAllowed != "" {
		list = append(list, fmt.Sprintf("Changes for machine %s are not supported: %s", spec.Machine.Name, notAllowed))
	}
	if notAllowed := notAllowedChanges(actual.bootstrapConfigPaths, allowed.bootstrapConfigPaths); notAllowed != "" {
		list = append(list, fmt.Sprintf("Changes for bootstrapConfig %s are not supported: %s", klog.KObj(spec.BootstrapConfig), notAllowed))
	}
	if notAllowed := notAllowedChanges(actual.infraMachinePaths, allowed.infraMachinePaths); notAllowed != "" {
		list = append(list, fmt.Sprintf("Changes for infraMachine %s are not supported: %s", klog.KObj(spec.InfraMachine), notAllowed))
	}

	return list
}

type machineSpedChanges struct {
	machinePaths         [][]string
	infraMachinePaths    [][]string
	bootstrapConfigPaths [][]string
}

func computeAllChanges(old, new *upgrade.MachineSpec) (*machineSpedChanges, error) {
	machineChanges, err := changedPaths(old.Machine.Spec, new.Machine.Spec)
	if err != nil {
		return nil, err
	}

	bootstrapChanges, err := changedPaths(old.BootstrapConfig.Object["spec"], new.BootstrapConfig.Object["spec"])
	if err != nil {
		return nil, err
	}

	infraChanges, err := changedPaths(old.InfraMachine.Object["spec"], new.InfraMachine.Object["spec"])
	if err != nil {
		return nil, err
	}

	return &machineSpedChanges{
		machinePaths:         machineChanges,
		infraMachinePaths:    infraChanges,
		bootstrapConfigPaths: bootstrapChanges,
	}, nil
}

func notAllowedChanges(actual, allowed [][]string) string {
	notAllowed := filterAllowed(actual, allowed)
	if len(notAllowed) == 0 {
		return ""
	}

	paths := make([]string, 0, len(notAllowed))
	for _, p := range notAllowed {
		paths = append(paths, strings.Join(p, "."))
	}
	return strings.Join(paths, "|")
}

func notAllowedPathChanged(old, new any, allowedPaths [][]string) (paths [][]string, err error) {
	changedPaths, err := changedPaths(old, new)
	if err != nil {
		return nil, err
	}

	return filterAllowed(changedPaths, allowedPaths), nil
}

func filterAllowed(changed, allowedPaths [][]string) [][]string {
	var paths [][]string
	for _, path := range changed {
		// Ignore paths that are empty
		if len(path) == 0 {
			continue
		}
		if !allowed(allowedPaths, path) {
			paths = append(paths, path)
		}
	}

	return paths
}

func changedPaths(old, new any) (changedPaths [][]string, err error) {
	originalJSON, err := json.Marshal(old)
	if err != nil {
		return nil, err
	}
	modifiedJSON, err := json.Marshal(new)
	if err != nil {
		return nil, err
	}

	diff, err := jsonpatch.CreateMergePatch(originalJSON, modifiedJSON)
	if err != nil {
		return nil, err
	}

	jsonPatch := map[string]interface{}{}
	if err := json.Unmarshal(diff, &jsonPatch); err != nil {
		return nil, err
	}
	// Build a list of all paths that are trying to change
	return paths([]string{}, jsonPatch), nil
}

// paths builds a slice of paths that are being modified.
func paths(path []string, diff map[string]interface{}) [][]string {
	allPaths := [][]string{}
	for key, m := range diff {
		nested, ok := m.(map[string]interface{})
		if !ok {
			// We have to use a copy of path, because otherwise the slice we append to
			// allPaths would be overwritten in another iteration.
			tmp := make([]string, len(path))
			copy(tmp, path)
			allPaths = append(allPaths, append(tmp, key))
			continue
		}
		allPaths = append(allPaths, paths(append(path, key), nested)...)
	}
	return allPaths
}

func allowed(allowList [][]string, path []string) bool {
	for _, allowed := range allowList {
		if pathsMatch(allowed, path) {
			return true
		}
	}
	return false
}

func pathsMatch(allowed, path []string) bool {
	// if either are empty then no match can be made
	if len(allowed) == 0 || len(path) == 0 {
		return false
	}
	i := 0
	for i = range path {
		// reached the end of the allowed path and no match was found
		if i > len(allowed)-1 {
			return false
		}
		if allowed[i] == "*" {
			return true
		}
		if path[i] != allowed[i] {
			return false
		}
	}
	// path has been completely iterated and has not matched the end of the path.
	// e.g. allowed: []string{"a","b","c"}, path: []string{"a"}
	return i >= len(allowed)-1
}
