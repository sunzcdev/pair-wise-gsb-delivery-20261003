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

// assertSameEffectiveContent asserts that two files have the same sections
// (including duplicate instance order), the same effective key set, raw
// values, and shadow order. Key order within a section is not significant.
func assertSameEffectiveContent(t *testing.T, want, got *File) {
	t.Helper()

	wantSecs := want.Sections()
	gotSecs := got.Sections()
	require.Len(t, gotSecs, len(wantSecs), "section count mismatch")
	for i := range wantSecs {
		assert.Equal(t, wantSecs[i].Name(), gotSecs[i].Name(), "section %d name", i)
		require.ElementsMatch(t, wantSecs[i].KeyStrings(), gotSecs[i].KeyStrings(),
			"section %q key set", wantSecs[i].Name())
		for _, kname := range wantSecs[i].KeyStrings() {
			wantKey := wantSecs[i].Key(kname)
			gotKey := gotSecs[i].Key(kname)
			assert.Equal(t, wantKey.ValueWithShadows(), gotKey.ValueWithShadows(),
				"key %q in section %q", kname, wantSecs[i].Name())
		}
	}
}

func TestDiff_BasicChanges(t *testing.T) {
	base, err := Load([]byte(`
[server]
host = localhost
port = 8080
old = gone

[removed]
key = value
`))
	require.NoError(t, err)

	modified, err := Load([]byte(`
[server]
host = example.com
port = 8080
tls = true

[added]
foo = bar
`))
	require.NoError(t, err)

	d := Diff(base, modified)
	require.NotNil(t, d)
	assert.False(t, d.Empty())

	// Deletions are reported separately, never as empty values.
	assert.Equal(t, []DeletionEntry{
		{Section: "server", Index: 0, Key: "old"},
		{Section: "removed", Index: 0},
	}, d.Deletions)

	// The overlay is ordinary INI content with only additions and changes.
	var buf bytes.Buffer
	_, err = d.Overlay.WriteTo(&buf)
	require.NoError(t, err)
	assert.Equal(t, `[server]
host = example.com
tls  = true

[added]
foo = bar
`, buf.String())

	// Applying the diff to the base restores the modified content.
	require.NoError(t, base.ApplyDiff(d))
	assertSameEffectiveContent(t, modified, base)
}

func TestDiff_NoDifference(t *testing.T) {
	data := []byte("[server]\nhost = localhost\n")
	base, err := Load(data)
	require.NoError(t, err)
	modified, err := Load(data)
	require.NoError(t, err)

	d := Diff(base, modified)
	assert.True(t, d.Empty())
	assert.Empty(t, d.Deletions)
}

func TestDiff_ShadowOrder(t *testing.T) {
	base, err := ShadowLoad([]byte(`
[server]
port = 1
port = 2
port = 3
`))
	require.NoError(t, err)

	// Same values but different shadow order must be detected.
	reordered, err := ShadowLoad([]byte(`
[server]
port = 3
port = 2
port = 1
`))
	require.NoError(t, err)

	d := Diff(base, reordered)
	assert.False(t, d.Empty(), "shadow order change must be detected")
	require.NoError(t, base.ApplyDiff(d))
	assert.Equal(t, []string{"3", "2", "1"}, base.Section("server").Key("port").ValueWithShadows())

	// Identical shadow order produces no diff.
	same, err := ShadowLoad([]byte(`
[server]
port = 3
port = 2
port = 1
`))
	require.NoError(t, err)
	assert.True(t, Diff(base, same).Empty())
}

func TestDiff_NonUniqueSections(t *testing.T) {
	opts := LoadOptions{AllowNonUniqueSections: true}
	base, err := LoadSources(opts, []byte(`
[item]
id = 1

[item]
id = 2

[item]
id = 3
`))
	require.NoError(t, err)

	// Instances are paired positionally: instances 2 and 3 changed.
	modified, err := LoadSources(opts, []byte(`
[item]
id = 1

[item]
id = 30

[item]
id = 4
`))
	require.NoError(t, err)

	d := Diff(base, modified)
	assert.Empty(t, d.Deletions)

	require.NoError(t, base.ApplyDiff(d))
	assertSameEffectiveContent(t, modified, base)

	// Duplicate instance order is preserved after applying.
	secs, err := base.SectionsByName("item")
	require.NoError(t, err)
	require.Len(t, secs, 3)
	assert.Equal(t, "1", secs[0].Key("id").Value())
	assert.Equal(t, "30", secs[1].Key("id").Value())
	assert.Equal(t, "4", secs[2].Key("id").Value())

	// Removing an instance produces a deletion entry, and applying it
	// restores the modified instance set.
	shrunk, err := LoadSources(opts, []byte(`
[item]
id = 1

[item]
id = 30
`))
	require.NoError(t, err)

	d = Diff(base, shrunk)
	assert.Equal(t, []DeletionEntry{
		{Section: "item", Index: 2},
	}, d.Deletions)

	require.NoError(t, base.ApplyDiff(d))
	assertSameEffectiveContent(t, shrunk, base)
}

func TestDiff_RawValuesOnly(t *testing.T) {
	// ValueMapper and interpolation must not be involved in the comparison.
	base, err := Load([]byte(`
[app]
name = world
greeting = hello %(name)s
`))
	require.NoError(t, err)
	modified, err := Load([]byte(`
[app]
name = world
greeting = hello %(name)s
`))
	require.NoError(t, err)

	panicMapper := func(string) string {
		panic("ValueMapper must not be called by Diff")
	}
	base.ValueMapper = panicMapper
	modified.ValueMapper = panicMapper

	d := Diff(base, modified)
	assert.True(t, d.Empty(), "identical raw values must produce an empty diff")

	// A change in raw value is detected even when interpolation would
	// produce the same final string.
	changed, err := Load([]byte(`
[app]
name = world
greeting = hello world
`))
	require.NoError(t, err)
	changed.ValueMapper = panicMapper

	d = Diff(base, changed)
	require.NotNil(t, d)
	assert.False(t, d.Empty(), "raw value change must be detected regardless of interpolation")
}

func TestDiff_EmptyValueIsNotDeletion(t *testing.T) {
	base, err := Load([]byte("[app]\nkey = value\n"))
	require.NoError(t, err)
	modified, err := Load([]byte("[app]\nkey =\n"))
	require.NoError(t, err)

	d := Diff(base, modified)
	assert.Empty(t, d.Deletions, "empty value is a change, not a deletion")
	assert.False(t, d.Empty())

	require.NoError(t, base.ApplyDiff(d))
	assert.Equal(t, "", base.Section("app").Key("key").Value())
	assert.True(t, base.Section("app").HasKey("key"))
}

func TestDiff_WithComments(t *testing.T) {
	base, err := Load([]byte("[app]\nkey = 1\n"))
	require.NoError(t, err)
	modified, err := Load([]byte("; section comment\n[app]\n; key comment\nkey = 2\n"))
	require.NoError(t, err)

	d := Diff(base, modified, DiffOptions{WithComments: true})
	var buf bytes.Buffer
	_, err = d.Overlay.WriteTo(&buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "; section comment")
	assert.Contains(t, buf.String(), "; key comment")

	// Comments are excluded by default.
	d = Diff(base, modified)
	buf.Reset()
	_, err = d.Overlay.WriteTo(&buf)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "comment")
}

func TestDiff_NestedAndBooleanKeys(t *testing.T) {
	opts := LoadOptions{AllowNestedValues: true, AllowBooleanKeys: true}
	base, err := LoadSources(opts, []byte(`
[app]
flag
list =
  a
  b
`))
	require.NoError(t, err)

	modified, err := LoadSources(opts, []byte(`
[app]
flag
list =
  a
  c
`))
	require.NoError(t, err)

	d := Diff(base, modified)
	assert.False(t, d.Empty(), "nested value change must be detected")
	require.NoError(t, base.ApplyDiff(d))
	assert.Equal(t, []string{"a", "c"}, base.Section("app").Key("list").NestedValues())
	assert.True(t, base.Section("app").Key("flag").isBooleanType)
}
