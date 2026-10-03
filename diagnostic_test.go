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
	Male   bool
	Money  float64
	Server diagServer
}

func TestMapToDiagnostics(t *testing.T) {
	t.Run("collect all independent errors with partial application", func(t *testing.T) {
		f, err := Load([]byte(`
Name = alice
Age = notanint
Male = notabool
Money = 1.5

[Server]
Host = example.com
Port = notaport
`))
		require.NoError(t, err)

		cfg := diagConfig{
			Name:   "keep",
			Age:    42,
			Male:   true,
			Server: diagServer{Host: "localhost", Port: 8080},
		}
		report := f.MapToDiagnostics(&cfg)
		require.NotNil(t, report)
		require.Len(t, report.Errors, 3)

		// Errors follow struct field definition order, not file order.
		assert.Equal(t, "Age", report.Errors[0].Path)
		assert.Equal(t, DefaultSection, report.Errors[0].Section)
		assert.Equal(t, "Age", report.Errors[0].Key)
		assert.NotEmpty(t, report.Errors[0].Reason)
		assert.NotNil(t, report.Errors[0].Err)

		assert.Equal(t, "Male", report.Errors[1].Path)
		assert.Equal(t, "Male", report.Errors[1].Key)

		assert.Equal(t, "Server.Port", report.Errors[2].Path)
		assert.Equal(t, "Server", report.Errors[2].Section)
		assert.Equal(t, "Port", report.Errors[2].Key)

		// Successful fields are written, failed fields keep previous values.
		assert.Equal(t, "alice", cfg.Name)
		assert.Equal(t, 1.5, cfg.Money)
		assert.Equal(t, "example.com", cfg.Server.Host)
		assert.Equal(t, 42, cfg.Age)
		assert.True(t, cfg.Male)
		assert.Equal(t, 8080, cfg.Server.Port)
	})

	t.Run("nil report when nothing fails", func(t *testing.T) {
		f, err := Load([]byte("Name = alice\nAge = 21\n"))
		require.NoError(t, err)

		var cfg diagConfig
		report := f.MapToDiagnostics(&cfg)
		assert.Nil(t, report)
		assert.Equal(t, "alice", cfg.Name)
		assert.Equal(t, 21, cfg.Age)
	})

	t.Run("not a pointer", func(t *testing.T) {
		f, err := Load([]byte(""))
		require.NoError(t, err)

		report := f.MapToDiagnostics(diagConfig{})
		require.NotNil(t, report)
		require.Len(t, report.Errors, 1)
		assert.Contains(t, report.Errors[0].Reason, "not a pointer")
	})

	t.Run("report implements error", func(t *testing.T) {
		f, err := Load([]byte("Age = bad\n"))
		require.NoError(t, err)

		var cfg diagConfig
		report := f.MapToDiagnostics(&cfg)
		require.NotNil(t, report)
		assert.Contains(t, report.Error(), "1 field mapping error(s)")
		assert.Contains(t, report.Error(), `field "Age"`)
		assert.Contains(t, report.Errors[0].Error(), `key "Age"`)
	})
}

func TestMapToDiagnostics_Slice(t *testing.T) {
	t.Run("element errors carry indices and keep previous value", func(t *testing.T) {
		f, err := Load([]byte("Ports = 1,2,bad,4,worse\n"))
		require.NoError(t, err)

		cfg := struct {
			Ports []int
		}{Ports: []int{9}}
		report := f.MapToDiagnostics(&cfg)
		require.NotNil(t, report)
		require.Len(t, report.Errors, 2)
		assert.Equal(t, "Ports[2]", report.Errors[0].Path)
		assert.Equal(t, "Ports[4]", report.Errors[1].Path)
		assert.Equal(t, "Ports", report.Errors[0].Key)
		assert.Equal(t, []int{9}, cfg.Ports)
	})

	t.Run("successful slice is written", func(t *testing.T) {
		f, err := Load([]byte("Ports = 1,2,3\n"))
		require.NoError(t, err)

		cfg := struct {
			Ports []int
		}{Ports: []int{9}}
		report := f.MapToDiagnostics(&cfg)
		assert.Nil(t, report)
		assert.Equal(t, []int{1, 2, 3}, cfg.Ports)
	})

	t.Run("unsupported element type reports parent only", func(t *testing.T) {
		f, err := Load([]byte("Values = 1,2,3\n"))
		require.NoError(t, err)

		cfg := struct {
			Values []int8
		}{}
		report := f.MapToDiagnostics(&cfg)
		require.NotNil(t, report)
		require.Len(t, report.Errors, 1)
		assert.Equal(t, "Values", report.Errors[0].Path)
		assert.Contains(t, report.Errors[0].Reason, "unsupported type")
	})
}

type diagItem struct {
	Name   string
	Weight int
}

func TestMapToDiagnostics_NonUniqueSections(t *testing.T) {
	t.Run("element errors carry indices", func(t *testing.T) {
		f, err := LoadSources(LoadOptions{AllowNonUniqueSections: true}, []byte(`
[item]
Name = a
Weight = 1

[item]
Name = b
Weight = bad
`))
		require.NoError(t, err)

		cfg := struct {
			Items []diagItem `ini:"item,nonunique"`
		}{}
		report := f.MapToDiagnostics(&cfg)
		require.NotNil(t, report)
		require.Len(t, report.Errors, 1)
		assert.Equal(t, "Items[1].Weight", report.Errors[0].Path)
		assert.Equal(t, "item", report.Errors[0].Section)
		assert.Equal(t, "Weight", report.Errors[0].Key)
		assert.Nil(t, cfg.Items)
	})

	t.Run("missing section reports parent only", func(t *testing.T) {
		f, err := LoadSources(LoadOptions{AllowNonUniqueSections: true}, []byte("Name = x\n"))
		require.NoError(t, err)

		cfg := struct {
			Items []diagItem `ini:"item,nonunique"`
		}{}
		report := f.MapToDiagnostics(&cfg)
		require.NotNil(t, report)
		require.Len(t, report.Errors, 1)
		assert.Equal(t, "Items", report.Errors[0].Path)
		assert.Empty(t, report.Errors[0].Key)
	})
}

func TestMapToDiagnostics_StableOrder(t *testing.T) {
	// Keys appear in the file in the opposite order of the struct fields;
	// the report must follow the struct definition order.
	f, err := Load([]byte("Gamma = bad\nBeta = bad\nAlpha = bad\n"))
	require.NoError(t, err)

	cfg := struct {
		Alpha int
		Beta  int
		Gamma int
	}{}
	report := f.MapToDiagnostics(&cfg)
	require.NotNil(t, report)
	require.Len(t, report.Errors, 3)
	assert.Equal(t, "Alpha", report.Errors[0].Path)
	assert.Equal(t, "Beta", report.Errors[1].Path)
	assert.Equal(t, "Gamma", report.Errors[2].Path)
}

func TestMapToDiagnostics_SectionMethod(t *testing.T) {
	f, err := Load([]byte("[Server]\nPort = bad\n"))
	require.NoError(t, err)

	cfg := diagConfig{Server: diagServer{Port: 8080}}
	report := f.Section("").MapToDiagnostics(&cfg)
	require.NotNil(t, report)
	require.Len(t, report.Errors, 1)
	assert.Equal(t, "Server.Port", report.Errors[0].Path)
	assert.Equal(t, 8080, cfg.Server.Port)
}
