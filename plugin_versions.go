// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PluginVersion describes a single version of a plugin (an individually
// named service within a plugin process).
//
// Different plugins inside one process can evolve on independent release
// cadences, so versions are negotiated per plugin name rather than for the
// process as a whole.
type PluginVersion struct {
	// Plugin is the concrete implementation served or consumed at this
	// version.
	Plugin Plugin

	// Required marks a plugin that must be present on the other side at a
	// mutually supported version. When false, the plugin is optional and is
	// simply omitted if no common version can be negotiated.
	Required bool
}

// PluginVersions maps a plugin name to its supported versions. The version
// numbers are application defined, like HandshakeConfig.ProtocolVersion.
type PluginVersions map[string]map[int]PluginVersion

// pluginVersionsEnvEntry is the parsed form of a single name entry in the
// PLUGIN_PLUGIN_VERSIONS environment variable.
type pluginVersionsEnvEntry struct {
	versions []int
	required bool
}

// validatePluginNames rejects plugin names that cannot be encoded in the
// handshake environment variable or response segment.
func validatePluginNames(versions PluginVersions) error {
	for name := range versions {
		if name == "" || strings.ContainsAny(name, ";=:,") {
			return fmt.Errorf("invalid plugin name %q: must not be empty or contain %q",
				name, ";=:,")
		}
	}
	return nil
}

// encodePluginVersions serializes a per-plugin version catalog into the value
// of the PLUGIN_PLUGIN_VERSIONS environment variable.
//
// Entries are emitted in sorted plugin name order so the result is
// deterministic. Each entry is:
//
//	name=<required-flag>:<version>,<version>
//
// where required-flag is 0 (optional) or 1 (required). Entries are separated
// by semicolons. Empty version sets are skipped.
func encodePluginVersions(versions PluginVersions) string {
	names := make([]string, 0, len(versions))
	for name := range versions {
		names = append(names, name)
	}
	sort.Strings(names)

	var entries []string
	for _, name := range names {
		byVersion := versions[name]
		if len(byVersion) == 0 {
			continue
		}

		var required bool
		vs := make([]int, 0, len(byVersion))
		for v, pv := range byVersion {
			vs = append(vs, v)
			if pv.Required {
				required = true
			}
		}
		sort.Sort(sort.Reverse(sort.IntSlice(vs)))

		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = strconv.Itoa(v)
		}

		flag := "0"
		if required {
			flag = "1"
		}
		entries = append(entries, fmt.Sprintf("%s=%s:%s", name, flag, strings.Join(parts, ",")))
	}

	return strings.Join(entries, ";")
}

// parsePluginVersionsEnv parses the value sent by a client in
// PLUGIN_PLUGIN_VERSIONS.
func parsePluginVersionsEnv(value string) (map[string]pluginVersionsEnvEntry, error) {
	parsed := make(map[string]pluginVersionsEnvEntry)
	if value == "" {
		return parsed, nil
	}

	for _, entry := range strings.Split(value, ";") {
		if entry == "" {
			continue
		}

		name, rest, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid plugin version entry %q", entry)
		}
		if strings.ContainsAny(name, ";=:,") {
			return nil, fmt.Errorf("invalid plugin name %q in plugin version entry", name)
		}

		flag, versionsRaw, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("invalid plugin version entry %q", entry)
		}

		var required bool
		switch flag {
		case "0":
			required = false
		case "1":
			required = true
		default:
			return nil, fmt.Errorf("invalid required flag %q in plugin version entry %q", flag, entry)
		}

		var versions []int
		for _, s := range strings.Split(versionsRaw, ",") {
			if s == "" {
				return nil, fmt.Errorf("invalid version in plugin version entry %q", entry)
			}
			v, err := strconv.Atoi(s)
			if err != nil {
				return nil, fmt.Errorf("invalid version %q in plugin version entry %q: %s", s, entry, err)
			}
			versions = append(versions, v)
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("no versions in plugin version entry %q", entry)
		}

		parsed[name] = pluginVersionsEnvEntry{versions: versions, required: required}
	}

	return parsed, nil
}

// parseNegotiatedPluginVersions parses the per-plugin version segment appended
// to the server handshake line. The segment is a comma separated list of
// name=version pairs, one entry per plugin on which the two sides agreed.
func parseNegotiatedPluginVersions(segment string) (map[string]int, error) {
	negotiated := make(map[string]int)
	if segment == "" {
		return negotiated, nil
	}

	for _, entry := range strings.Split(segment, ",") {
		name, versionRaw, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid negotiated plugin version entry %q", entry)
		}
		version, err := strconv.Atoi(versionRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid negotiated version %q for plugin %q: %s", versionRaw, name, err)
		}
		negotiated[name] = version
	}

	return negotiated, nil
}

// encodeNegotiatedPluginVersions serializes the negotiated name=version pairs,
// in sorted plugin name order.
func encodeNegotiatedPluginVersions(negotiated map[string]int) string {
	names := make([]string, 0, len(negotiated))
	for name := range negotiated {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, fmt.Sprintf("%s=%d", name, negotiated[name]))
	}
	return strings.Join(entries, ",")
}

// negotiatePlugins computes the highest mutually supported version for each
// plugin name independently.
//
// A plugin present on only one side is dropped when it is optional on that
// side; it returns an error when it is required there. A plugin present on
// both sides but with no overlapping version always returns an error, since a
// plugin that has been explicitly versioned on both sides but cannot agree is
// treated as required at the intersection point.
func negotiatePlugins(
	local PluginVersions,
	remote map[string]pluginVersionsEnvEntry,
) (map[string]int, error) {
	negotiated := make(map[string]int)

	// Local plugins: must intersect with the remote catalog, unless they are
	// optional and may be dropped.
	for name, localByVersion := range local {
		highest, found := highestCommonVersion(localByVersion, remote[name].versions)
		if found {
			negotiated[name] = highest
			continue
		}

		if localRequired(localByVersion) {
			return nil, fmt.Errorf(
				"incompatible API version for required plugin %q: local versions %s, remote versions %v",
				name, localVersionList(localByVersion), remote[name].versions)
		}
		// Optional plugin with no common version: omit it.
	}

	// Remote plugins not offered locally: fail only if the remote side marked
	// them required.
	for name, remoteEntry := range remote {
		if _, offered := local[name]; offered {
			continue
		}
		if remoteEntry.required {
			return nil, fmt.Errorf(
				"required plugin %q is supported by the remote side but not local", name)
		}
	}

	return negotiated, nil
}

// highestCommonVersion returns the largest version number present in both the
// local version catalog and the remote version list.
func highestCommonVersion(local map[int]PluginVersion, remote []int) (int, bool) {
	remoteSet := make(map[int]struct{}, len(remote))
	for _, v := range remote {
		remoteSet[v] = struct{}{}
	}

	best := 0
	found := false
	for v := range local {
		if _, ok := remoteSet[v]; !ok {
			continue
		}
		if !found || v > best {
			best = v
			found = true
		}
	}
	return best, found
}

// localRequired reports whether any version entry for a plugin is required.
func localRequired(byVersion map[int]PluginVersion) bool {
	for _, pv := range byVersion {
		if pv.Required {
			return true
		}
	}
	return false
}

// localVersionList renders a local version catalog as a sorted, comma
// separated list for error messages.
func localVersionList(byVersion map[int]PluginVersion) string {
	vs := make([]int, 0, len(byVersion))
	for v := range byVersion {
		vs = append(vs, v)
	}
	sort.Ints(vs)
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// pluginSetFromNegotiated resolves the negotiated name=version pairs against a
// local version catalog, producing the PluginSet that should be registered or
// dispensed.
func pluginSetFromNegotiated(catalog PluginVersions, negotiated map[string]int) PluginSet {
	set := make(PluginSet, len(negotiated))
	for name, version := range negotiated {
		if byVersion, ok := catalog[name]; ok {
			if pv, ok := byVersion[version]; ok {
				set[name] = pv.Plugin
			}
		}
	}
	return set
}

// globalVersionGroups flattens a per-plugin catalog into the legacy global
// version to PluginSet grouping used by the version list handshake. Each
// global version group contains every plugin that offers that version.
func globalVersionGroups(versions PluginVersions) map[int]PluginSet {
	groups := make(map[int]PluginSet)
	for name, byVersion := range versions {
		for v, pv := range byVersion {
			if groups[v] == nil {
				groups[v] = make(PluginSet)
			}
			groups[v][name] = pv.Plugin
		}
	}
	return groups
}

// pluginSetForGlobalGroup returns the subset of a per-plugin catalog that
// offers the given global version. It is used when the peer is old and only
// negotiates one global version: required plugins that do not offer that
// version force a failure, optional ones are dropped.
func pluginSetForGlobalGroup(versions PluginVersions, globalVersion int) (PluginSet, error) {
	set := make(PluginSet)
	for name, byVersion := range versions {
		pv, ok := byVersion[globalVersion]
		if !ok {
			if localRequired(byVersion) {
				return nil, fmt.Errorf(
					"required plugin %q does not support negotiated protocol version %d",
					name, globalVersion)
			}
			continue
		}
		set[name] = pv.Plugin
	}
	return set, nil
}

// transportForPlugins reports whether the plugin set contains a gRPC plugin,
// matching the existing rule that all plugins in a set share one transport.
func transportForPlugins(set PluginSet) Protocol {
	for _, p := range set {
		if _, ok := p.(GRPCPlugin); ok {
			return ProtocolGRPC
		}
		break
	}
	return ProtocolNetRPC
}
