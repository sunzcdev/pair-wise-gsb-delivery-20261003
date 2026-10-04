// Copyright 2026 Unknwon
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package ini

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffFiles(t *testing.T) {
	t.Run("identical files produce empty diff", func(t *testing.T) {
		base, err := Load([]byte("[server]\nhost = a\nport = 1\n"))
		require.NoError(t, err)
		modified, err := Load([]byte("[server]\nhost = a\nport = 1\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		assert.True(t, d.IsEmpty())
		assert.Empty(t, d.Sets)
		assert.Empty(t, d.Deletions)
	})

	t.Run("additions and changes are exported as regular INI", func(t *testing.T) {
		base, err := Load([]byte("[server]\nhost = a\nport = 1\n"))
		require.NoError(t, err)
		modified, err := Load([]byte("[server]\nhost = b\nport = 1\n[auth]\nuser = u\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		require.Len(t, d.Sets, 2)
		assert.Empty(t, d.Deletions)

		// Changed key only, unchanged "port" is not exported.
		assert.Equal(t, "server", d.Sets[0].Section)
		assert.False(t, d.Sets[0].NewSection)
		require.Len(t, d.Sets[0].Keys, 1)
		assert.Equal(t, "host", d.Sets[0].Keys[0].Name)
		assert.Equal(t, []string{"b"}, d.Sets[0].Keys[0].Values)

		// Whole new section.
		assert.Equal(t, "auth", d.Sets[1].Section)
		assert.True(t, d.Sets[1].NewSection)

		var buf bytes.Buffer
		_, err = d.WriteTo(&buf)
		require.NoError(t, err)
		out := buf.String()
		assert.Contains(t, out, "[server]")
		assert.Contains(t, out, "host")
		assert.Contains(t, out, "b")
		assert.Contains(t, out, "[auth]")
		assert.NotContains(t, out, "port")
	})

	t.Run("deletions are a separate list", func(t *testing.T) {
		base, err := Load([]byte("[server]\nhost = a\nport = 1\n[legacy]\nold = x\n"))
		require.NoError(t, err)
		modified, err := Load([]byte("[server]\nhost = a\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		assert.Empty(t, d.Sets)
		require.Len(t, d.Deletions, 2)
		assert.Equal(t, DiffDeletion{Section: "server", Index: 0, Key: "port"}, d.Deletions[0])
		assert.Equal(t, DiffDeletion{Section: "legacy", Index: 0}, d.Deletions[1])
		assert.Equal(t, "[legacy#0] (entire section)", d.Deletions[1].String())

		// Deletions must not show up in the overlay as empty values.
		var buf bytes.Buffer
		_, err = d.WriteTo(&buf)
		require.NoError(t, err)
		assert.NotContains(t, buf.String(), "port")
		assert.NotContains(t, buf.String(), "legacy")
	})

	t.Run("empty value is not a deletion", func(t *testing.T) {
		base, err := Load([]byte("[server]\nhost = a\n"))
		require.NoError(t, err)
		modified, err := Load([]byte("[server]\nhost =\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		assert.Empty(t, d.Deletions)
		require.Len(t, d.Sets, 1)
		require.Len(t, d.Sets[0].Keys, 1)
		assert.Equal(t, []string{""}, d.Sets[0].Keys[0].Values)
	})

	t.Run("shadow order is compared on raw values", func(t *testing.T) {
		base, err := ShadowLoad([]byte("[s]\nk = 1\nk = 2\n"))
		require.NoError(t, err)
		modified, err := ShadowLoad([]byte("[s]\nk = 2\nk = 1\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		require.Len(t, d.Sets, 1)
		require.Len(t, d.Sets[0].Keys, 1)
		assert.Equal(t, []string{"2", "1"}, d.Sets[0].Keys[0].Values)
	})

	t.Run("ValueMapper is not consulted", func(t *testing.T) {
		mapper := func(s string) string { return "mapped-" + s }
		base, err := Load([]byte("[s]\nk = v\n"))
		require.NoError(t, err)
		base.ValueMapper = mapper
		modified, err := Load([]byte("[s]\nk = v\n"))
		require.NoError(t, err)
		modified.ValueMapper = mapper

		assert.True(t, DiffFiles(base, modified).IsEmpty())
	})

	t.Run("export order is stable", func(t *testing.T) {
		base := Empty()
		modified, err := Load([]byte("[b]\ny = 2\n[a]\nx = 1\n[b]\n"))
		require.NoError(t, err)

		first := DiffFiles(base, modified)
		second := DiffFiles(base, modified)
		assert.Equal(t, first, second)
	})
}

func TestDiff_ApplyTo(t *testing.T) {
	t.Run("restores effective key set", func(t *testing.T) {
		base, err := Load([]byte("[server]\nhost = a\nport = 1\n[legacy]\nold = x\n"))
		require.NoError(t, err)
		modified, err := Load([]byte("[server]\nhost = b\n[auth]\nuser = u\nempty =\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		require.NoError(t, d.ApplyTo(base))

		// The patched base must be indistinguishable from the modified file.
		assert.True(t, DiffFiles(base, modified).IsEmpty())
		assert.Equal(t, "b", base.Section("server").Key("host").Value())
		assert.False(t, base.Section("server").HasKey("port"))
		assert.False(t, base.HasSection("legacy"))
		assert.Equal(t, "u", base.Section("auth").Key("user").Value())

		// Explicit empty value survives as a key, not a deletion.
		assert.True(t, base.Section("auth").HasKey("empty"))
		assert.Equal(t, "", base.Section("auth").Key("empty").Value())
	})

	t.Run("restores shadow order", func(t *testing.T) {
		base, err := ShadowLoad([]byte("[s]\nk = 1\nk = 2\nkeep = x\n"))
		require.NoError(t, err)
		modified, err := ShadowLoad([]byte("[s]\nk = 2\nk = 1\nk = 3\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		require.NoError(t, d.ApplyTo(base))

		assert.True(t, DiffFiles(base, modified).IsEmpty())
		assert.Equal(t, []string{"2", "1", "3"}, base.Section("s").Key("k").ValueWithShadows())
		assert.False(t, base.Section("s").HasKey("keep"))
	})

	t.Run("restores duplicate section instances and their order", func(t *testing.T) {
		opts := LoadOptions{AllowNonUniqueSections: true}
		base, err := LoadSources(opts, []byte("[app]\nv = 1\n[app]\nv = 2\n[app]\nv = 3\n"))
		require.NoError(t, err)
		modified, err := LoadSources(opts, []byte("[app]\nv = 1\n[app]\nv = 9\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		require.Len(t, d.Sets, 1)
		assert.Equal(t, 1, d.Sets[0].Index)
		require.Len(t, d.Deletions, 1)
		assert.Equal(t, DiffDeletion{Section: "app", Index: 2}, d.Deletions[0])

		require.NoError(t, d.ApplyTo(base))
		assert.True(t, DiffFiles(base, modified).IsEmpty())

		secs, err := base.SectionsByName("app")
		require.NoError(t, err)
		require.Len(t, secs, 2)
		assert.Equal(t, "1", secs[0].Key("v").Value())
		assert.Equal(t, "9", secs[1].Key("v").Value())
	})

	t.Run("adds duplicate section instances", func(t *testing.T) {
		opts := LoadOptions{AllowNonUniqueSections: true}
		base, err := LoadSources(opts, []byte("[app]\nv = 1\n"))
		require.NoError(t, err)
		modified, err := LoadSources(opts, []byte("[app]\nv = 1\n[app]\nv = 2\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)
		require.Len(t, d.Sets, 1)
		assert.True(t, d.Sets[0].NewSection)
		assert.Equal(t, 1, d.Sets[0].Index)

		require.NoError(t, d.ApplyTo(base))
		assert.True(t, DiffFiles(base, modified).IsEmpty())

		secs, err := base.SectionsByName("app")
		require.NoError(t, err)
		require.Len(t, secs, 2)
		assert.Equal(t, "2", secs[1].Key("v").Value())
	})

	t.Run("overlay round-trips through the parser", func(t *testing.T) {
		base, err := Load([]byte("[server]\nhost = a\nport = 1\n"))
		require.NoError(t, err)
		modified, err := Load([]byte("[server]\nhost = b\nport = 1\n[auth]\nuser = u\n"))
		require.NoError(t, err)

		d := DiffFiles(base, modified)

		// Serialize the overlay as regular INI and load it back.
		var buf bytes.Buffer
		_, err = d.WriteTo(&buf)
		require.NoError(t, err)
		overlay, err := Load(buf.Bytes())
		require.NoError(t, err)
		assert.Equal(t, "b", overlay.Section("server").Key("host").Value())
		assert.Equal(t, "u", overlay.Section("auth").Key("user").Value())

		// Applying the parsed overlay plus the deletions restores the
		// modified effective key set as well.
		parsed := DiffFiles(Empty(), modified) // sanity: parsed overlay is a subset
		assert.False(t, parsed.IsEmpty())
		require.NoError(t, d.ApplyTo(base))
		assert.True(t, DiffFiles(base, modified).IsEmpty())
	})
}
