package pluginmanifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
)

func Decode(raw []byte) (Document, error) {
	if !json.Valid(raw) {
		return Document{}, errors.New("plugin manifest is not valid JSON")
	}
	var header struct {
		SpecVersion string `json:"specVersion"`
		APIVersion  string `json:"apiVersion"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return Document{}, err
	}
	switch {
	case header.SpecVersion == SpecV2:
		var manifest Manifest
		if err := strictDecode(raw, &manifest); err != nil {
			return Document{}, fmt.Errorf("decode v2 manifest: %w", err)
		}
		manifest = normalize(manifest)
		if err := manifest.Validate(); err != nil {
			return Document{}, err
		}
		return Document{Manifest: manifest, SourceVersion: SourceV2}, nil
	case header.SpecVersion == "" && header.APIVersion == LegacySpecV1:
		var legacy LegacyManifest
		if err := strictDecode(raw, &legacy); err != nil {
			return Document{}, fmt.Errorf("decode v1 manifest: %w", err)
		}
		if err := validateLegacy(legacy); err != nil {
			return Document{}, err
		}
		manifest := normalize(adaptLegacy(legacy))
		if err := manifest.Validate(); err != nil {
			return Document{}, fmt.Errorf("adapt v1 manifest: %w", err)
		}
		return Document{Manifest: manifest, SourceVersion: SourceV1, Legacy: &legacy}, nil
	default:
		version := header.SpecVersion
		if version == "" {
			version = header.APIVersion
		}
		return Document{}, fmt.Errorf("unsupported plugin manifest version %q", version)
	}
}

func strictDecode(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest contains multiple JSON values")
		}
		return err
	}
	return nil
}

func validateLegacy(legacy LegacyManifest) error {
	if legacy.APIVersion != LegacySpecV1 || !idPattern.MatchString(legacy.ID) {
		return errors.New("invalid axiom.plugin/v1 identity")
	}
	if legacy.Name == "" || legacy.Description == "" || !versionPattern.MatchString(legacy.Version) {
		return errors.New("invalid axiom.plugin/v1 metadata")
	}
	if legacy.Backend.Artifact == "" || legacy.Backend.Protocol != "axiom.rpc/v1" || legacy.Frontend.Entry == "" {
		return errors.New("axiom.plugin/v1 requires backend and frontend entries")
	}
	if len(legacy.Capabilities) == 0 {
		return errors.New("axiom.plugin/v1 requires at least one capability")
	}
	return nil
}

func adaptLegacy(legacy LegacyManifest) Manifest {
	read := []string{}
	write := []string{}
	if legacy.Permissions.WorkspaceRead {
		read = append(read, "${workspace}")
	}
	if legacy.Permissions.PluginDataWrite {
		write = append(write, "${pluginData}")
	}
	tools := make([]ToolExport, 0, len(legacy.Capabilities))
	for _, capability := range legacy.Capabilities {
		tools = append(tools, ToolExport{
			ID:           capability.ID,
			Summary:      capability.Summary,
			Visibility:   "discoverable",
			Risk:         capability.Risk,
			Executor:     Executor{Kind: "backend", Target: "capability.invoke"},
			InputSchema:  append(json.RawMessage(nil), capability.InputSchema...),
			OutputSchema: append(json.RawMessage(nil), capability.OutputSchema...),
		})
	}
	return Manifest{
		SpecVersion: SpecV2,
		ID:          legacy.ID,
		Name:        legacy.Name,
		Version:     legacy.Version,
		Description: legacy.Description,
		Runtime: &Runtime{Backend: &Backend{
			Artifact:       legacy.Backend.Artifact,
			Protocol:       legacy.Backend.Protocol,
			ShutdownMillis: legacy.Backend.ShutdownMillis,
		}},
		UI: &UI{
			Entry:   legacy.Frontend.Entry,
			Assets:  legacyAssetPattern(legacy.Frontend.Entry),
			Slots:   append([]string(nil), legacy.Frontend.Slots...),
			Sandbox: "strict",
		},
		Exports: Exports{Tools: tools},
		Permissions: Permissions{
			Filesystem: FilesystemPermissions{Read: read, Write: write},
			Network:    append([]string(nil), legacy.Permissions.Network...),
			Secrets:    append([]string(nil), legacy.Permissions.Secrets...),
			Background: legacy.Permissions.Background,
		},
		Upgrade: Upgrade{
			Strategy:       legacy.Upgrade.Strategy,
			PinActiveCalls: legacy.Upgrade.PinActiveRuns,
			StateVersion:   legacy.Upgrade.StateVersion,
		},
	}
}

func normalize(manifest Manifest) Manifest {
	if manifest.Runtime != nil && manifest.Runtime.Backend != nil && manifest.Runtime.Backend.ShutdownMillis == 0 {
		manifest.Runtime.Backend.ShutdownMillis = 10000
	}
	if manifest.UI != nil {
		if manifest.UI.Sandbox == "" {
			manifest.UI.Sandbox = "strict"
		}
		if manifest.UI.Assets == "" {
			manifest.UI.Assets = legacyAssetPattern(manifest.UI.Entry)
		}
	}
	if manifest.Upgrade.Strategy == "" {
		manifest.Upgrade.Strategy = "drain"
	}
	if manifest.Upgrade.StateVersion == 0 {
		manifest.Upgrade.StateVersion = 1
	}
	return manifest
}

func legacyAssetPattern(entry string) string {
	directory := path.Dir(entry)
	if directory == "." {
		return "*"
	}
	return directory + "/**"
}
