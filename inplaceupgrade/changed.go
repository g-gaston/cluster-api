package inplaceupgrade

import (
	"encoding/json"

	jsonpatch "github.com/evanphx/json-patch"
)

func notAllowedPathChanged(old, new any, allowedPaths [][]string) (paths [][]string, err error) {
	changedPaths, err := changedPaths(old, new)
	if err != nil {
		return nil, err
	}

	for _, path := range changedPaths {
		// Ignore paths that are empty
		if len(path) == 0 {
			continue
		}
		if !allowed(allowedPaths, path) {
			paths = append(paths, path)
		}
	}

	return paths, nil
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
