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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type diagServer struct {
	Host string
	Port int
}

type diagConfig struct {
	Name   string
	Age    int
	Money  float64
	Ports  []int
	Server diagServer
}

// Note: keys are intentionally ordered differently than the struct fields
// to prove that the report order follows the struct definition.
const confDiagnostic = `
Ports = 80, oops, 443
Name = gopher
Age = not-an-int
Money = 1.25

[Server]
Host = example.com
Port = not-a-port
`

func Test_MapToWithDiagnostics(t *testing.T) {
	f, err := Load([]byte(confDiagnostic))
	require.NoError(t, err)

	cfg := diagConfig{
		Age:    42,
		Ports:  []int{1},
		Server: diagServer{Host: "old", Port: 8080},
	}
	report := f.MapToWithDiagnostics(&cfg)
	require.NotNil(t, report)
	require.True(t, report.HasErrors())

	// All independently discoverable errors are reported at once, in the
	// order the fields are declared in the struct definition.
	require.Len(t, report.Errors, 3)

	assert.Equal(t, "Age", report.Errors[0].Path)
	assert.Equal(t, DefaultSection, report.Errors[0].Section)
	assert.Equal(t, "Age", report.Errors[0].Key)
	assert.NotEmpty(t, report.Errors[0].Reason)

	assert.Equal(t, "Ports[1]", report.Errors[1].Path)
	assert.Equal(t, DefaultSection, report.Errors[1].Section)
	assert.Equal(t, "Ports", report.Errors[1].Key)

	assert.Equal(t, "Server.Port", report.Errors[2].Path)
	assert.Equal(t, "Server", report.Errors[2].Section)
	assert.Equal(t, "Port", report.Errors[2].Key)

	// Partial application: successful fields are written, failed fields
	// keep the values they had before the call.
	assert.Equal(t, "gopher", cfg.Name)
	assert.Equal(t, 42, cfg.Age)
	assert.Equal(t, 1.25, cfg.Money)
	assert.Equal(t, []int{1}, cfg.Ports)
	assert.Equal(t, "example.com", cfg.Server.Host)
	assert.Equal(t, 8080, cfg.Server.Port)
}

func Test_MapToWithDiagnostics_noErrors(t *testing.T) {
	f, err := Load([]byte(`
Name = gopher
Age = 21
Money = 1.25
Ports = 80, 443

[Server]
Host = example.com
Port = 8080
`))
	require.NoError(t, err)

	var cfg diagConfig
	report := f.MapToWithDiagnostics(&cfg)
	require.NotNil(t, report)
	assert.False(t, report.HasErrors())
	assert.Equal(t, "ini: mapping completed without errors", report.String())
	assert.Equal(t, diagConfig{
		Name:   "gopher",
		Age:    21,
		Money:  1.25,
		Ports:  []int{80, 443},
		Server: diagServer{Host: "example.com", Port: 8080},
	}, cfg)
}

type diagPeer struct {
	Name string
	Port int
}

type diagPeers struct {
	Peers  []diagPeer `ini:",nonunique"`
	Ghosts []diagPeer `ini:",nonunique"`
}

func Test_MapToWithDiagnostics_nonUniqueSections(t *testing.T) {
	f, err := LoadSources(LoadOptions{AllowNonUniqueSections: true}, []byte(`
[Peers]
Name = a
Port = 1

[Peers]
Name = b
Port = bad
`))
	require.NoError(t, err)

	var v diagPeers
	report := f.MapToWithDiagnostics(&v)
	require.True(t, report.HasErrors())
	require.Len(t, report.Errors, 2)

	// Slice element errors carry the element index.
	assert.Equal(t, "Peers[1].Port", report.Errors[0].Path)
	assert.Equal(t, "Peers", report.Errors[0].Section)
	assert.Equal(t, "Port", report.Errors[0].Key)

	// A parent field that fails (no matching sections) reports a single
	// error without fabricating errors for its child fields.
	assert.Equal(t, "Ghosts", report.Errors[1].Path)
	assert.Equal(t, "Ghosts", report.Errors[1].Section)
	assert.Empty(t, report.Errors[1].Key)

	// Elements that converted successfully are still applied.
	require.Len(t, v.Peers, 2)
	assert.Equal(t, "a", v.Peers[0].Name)
	assert.Equal(t, 1, v.Peers[0].Port)
	assert.Equal(t, "b", v.Peers[1].Name)
	assert.Equal(t, 0, v.Peers[1].Port)
	assert.Empty(t, v.Ghosts)
}

func Test_MapToWithDiagnostics_reportString(t *testing.T) {
	f, err := Load([]byte(`Age = not-an-int`))
	require.NoError(t, err)

	var cfg diagConfig
	report := f.MapToWithDiagnostics(&cfg)
	require.True(t, report.HasErrors())
	assert.Contains(t, report.String(), "successful fields were applied; failed fields kept their previous values")
	assert.Contains(t, report.Errors[0].Error(), `field "Age"`)
	assert.Contains(t, report.Errors[0].Error(), `key "Age"`)
}

func Test_MapToWithDiagnostics_failFastUnchanged(t *testing.T) {
	// The diagnostic mode must not change the existing fail-fast behavior
	// of MapTo and StrictMapTo.
	f, err := Load([]byte(confDiagnostic))
	require.NoError(t, err)

	var cfg diagConfig
	assert.NoError(t, f.MapTo(&cfg))

	err = f.StrictMapTo(&cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Age")
}
