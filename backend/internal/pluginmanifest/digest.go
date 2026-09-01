package pluginmanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func SurfaceKeys(manifest Manifest) []string {
	keys := []string{}
	if manifest.Runtime != nil && manifest.Runtime.Backend != nil {
		keys = append(keys, "runtime:backend")
	}
	if manifest.UI != nil {
		keys = append(keys, "ui:entry:"+manifest.UI.Entry)
		for _, slot := range manifest.UI.Slots {
			keys = append(keys, "ui:slot:"+slot)
		}
	}
	for _, service := range manifest.Exports.Services {
		keys = append(keys, "service:"+service.ID+":"+service.Contract)
	}
	for _, tool := range manifest.Exports.Tools {
		keys = append(keys, "tool:"+tool.ID+":"+tool.Visibility)
	}
	for _, skill := range manifest.Exports.Skills {
		keys = append(keys, "skill:"+skill.ID+":"+skill.Visibility)
	}
	for _, hook := range manifest.Exports.Hooks {
		keys = append(keys, "hook:"+hook.ID)
		for _, event := range hook.Events {
			keys = append(keys, "hook-event:"+hook.ID+":"+event)
		}
	}
	for _, job := range manifest.Exports.Jobs {
		keys = append(keys, "job:"+job.ID)
	}
	return canonicalStrings(keys)
}

func SurfaceDigest(manifest Manifest) string {
	raw, _ := json.Marshal(SurfaceKeys(manifest))
	return sha256Hex(raw)
}

// GrantDigest binds approval to canonical permissions and the exported surface
// set. The immutable release digest remains a separate installation binding.
func GrantDigest(manifest Manifest) string {
	permissions := manifest.Permissions
	permissions.Filesystem.Read = canonicalStrings(permissions.Filesystem.Read)
	permissions.Filesystem.Write = canonicalStrings(permissions.Filesystem.Write)
	permissions.Network = canonicalStrings(permissions.Network)
	permissions.Secrets = canonicalStrings(permissions.Secrets)
	payload := struct {
		Permissions Permissions `json:"permissions"`
		Surfaces    []string    `json:"surfaces"`
	}{Permissions: permissions, Surfaces: SurfaceKeys(manifest)}
	raw, _ := json.Marshal(payload)
	return sha256Hex(raw)
}

func canonicalStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	copyValues := append([]string(nil), values...)
	sort.Strings(copyValues)
	result := copyValues[:0]
	for _, value := range copyValues {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
