// Copyright (c) 2026 Kelsey Hightower and others. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package envconfig

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

type MigrationSpecification struct {
	RequestTimeout time.Duration `envconfig:"REQUEST_TIMEOUT" aliases:"OLD_TIMEOUT,LEGACY_TIMEOUT" deprecated:"OLD_TIMEOUT"`
}

func findField(t *testing.T, r *Report, name string) FieldReport {
	t.Helper()
	for _, f := range r.Fields {
		if f.FieldName == name {
			return f
		}
	}
	t.Fatalf("no report entry for field %s", name)
	return FieldReport{}
}

func countWarnings(warnings []string, substr string) int {
	n := 0
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			n++
		}
	}
	return n
}

func TestAliasFallbackAndReport(t *testing.T) {
	var s MigrationSpecification
	os.Clearenv()
	os.Setenv("ENV_CONFIG_OLD_TIMEOUT", "5s")

	report, err := ProcessWithReport("env_config", &s)
	if err != nil {
		t.Fatal(err)
	}
	if s.RequestTimeout != 5*time.Second {
		t.Errorf("expected %s, got %s", 5*time.Second, s.RequestTimeout)
	}

	f := findField(t, report, "RequestTimeout")
	if f.Key != "ENV_CONFIG_REQUEST_TIMEOUT" {
		t.Errorf("expected canonical key ENV_CONFIG_REQUEST_TIMEOUT, got %s", f.Key)
	}
	if f.UsedKey != "ENV_CONFIG_OLD_TIMEOUT" {
		t.Errorf("expected used key ENV_CONFIG_OLD_TIMEOUT, got %s", f.UsedKey)
	}
	if !f.Deprecated {
		t.Error("expected Deprecated to be true when value comes from a deprecated name")
	}

	if n := countWarnings(report.Warnings, "ENV_CONFIG_OLD_TIMEOUT"); n != 1 {
		t.Errorf("expected exactly one deprecation warning for ENV_CONFIG_OLD_TIMEOUT, got %d: %v", n, report.Warnings)
	}
	if !strings.Contains(report.Warnings[0], "ENV_CONFIG_REQUEST_TIMEOUT") {
		t.Errorf("expected deprecation warning to point at ENV_CONFIG_REQUEST_TIMEOUT, got %q", report.Warnings[0])
	}
}

func TestAliasPriorityOrder(t *testing.T) {
	var s MigrationSpecification
	os.Clearenv()
	os.Setenv("ENV_CONFIG_LEGACY_TIMEOUT", "9s")

	report, err := ProcessWithReport("env_config", &s)
	if err != nil {
		t.Fatal(err)
	}
	if s.RequestTimeout != 9*time.Second {
		t.Errorf("expected %s, got %s", 9*time.Second, s.RequestTimeout)
	}
	if f := findField(t, report, "RequestTimeout"); f.UsedKey != "ENV_CONFIG_LEGACY_TIMEOUT" {
		t.Errorf("expected used key ENV_CONFIG_LEGACY_TIMEOUT, got %s", f.UsedKey)
	}

	// OLD_TIMEOUT is declared before LEGACY_TIMEOUT, so it wins.
	os.Clearenv()
	os.Setenv("ENV_CONFIG_OLD_TIMEOUT", "5s")
	os.Setenv("ENV_CONFIG_LEGACY_TIMEOUT", "9s")

	report, err = ProcessWithReport("env_config", &s)
	if err != nil {
		t.Fatal(err)
	}
	if s.RequestTimeout != 5*time.Second {
		t.Errorf("expected %s, got %s", 5*time.Second, s.RequestTimeout)
	}
	if f := findField(t, report, "RequestTimeout"); f.UsedKey != "ENV_CONFIG_OLD_TIMEOUT" {
		t.Errorf("expected used key ENV_CONFIG_OLD_TIMEOUT, got %s", f.UsedKey)
	}
}

func TestAliasCanonicalWinsOnConflict(t *testing.T) {
	var s MigrationSpecification
	os.Clearenv()
	os.Setenv("ENV_CONFIG_REQUEST_TIMEOUT", "1s")
	os.Setenv("ENV_CONFIG_OLD_TIMEOUT", "5s")

	report, err := ProcessWithReport("env_config", &s)
	if err != nil {
		t.Fatal(err)
	}
	if s.RequestTimeout != time.Second {
		t.Errorf("expected %s, got %s", time.Second, s.RequestTimeout)
	}

	f := findField(t, report, "RequestTimeout")
	if f.UsedKey != "ENV_CONFIG_REQUEST_TIMEOUT" {
		t.Errorf("expected used key ENV_CONFIG_REQUEST_TIMEOUT, got %s", f.UsedKey)
	}
	if f.Deprecated {
		t.Error("expected Deprecated to be false when value comes from the canonical key")
	}
	if n := countWarnings(report.Warnings, "ENV_CONFIG_OLD_TIMEOUT is deprecated"); n != 1 {
		t.Errorf("expected exactly one deprecation warning for ENV_CONFIG_OLD_TIMEOUT, got %d: %v", n, report.Warnings)
	}
}

func TestAliasConflictErrorMode(t *testing.T) {
	var s MigrationSpecification
	os.Clearenv()
	os.Setenv("ENV_CONFIG_REQUEST_TIMEOUT", "1s")
	os.Setenv("ENV_CONFIG_OLD_TIMEOUT", "5s")

	_, err := ProcessWithReport("env_config", &s, WithConflictMode(ConflictError))
	if err == nil {
		t.Fatal("expected conflict error, got none")
	}
	if !strings.Contains(err.Error(), "ENV_CONFIG_REQUEST_TIMEOUT") || !strings.Contains(err.Error(), "ENV_CONFIG_OLD_TIMEOUT") {
		t.Errorf("expected error to mention both keys, got %q", err)
	}
}

func TestAliasSameValueSingleDeprecationWarning(t *testing.T) {
	var s MigrationSpecification
	os.Clearenv()
	os.Setenv("ENV_CONFIG_REQUEST_TIMEOUT", "5s")
	os.Setenv("ENV_CONFIG_OLD_TIMEOUT", "5s")

	report, err := ProcessWithReport("env_config", &s, WithConflictMode(ConflictError))
	if err != nil {
		t.Fatalf("identical values must not conflict, got %v", err)
	}
	if s.RequestTimeout != 5*time.Second {
		t.Errorf("expected %s, got %s", 5*time.Second, s.RequestTimeout)
	}
	if n := countWarnings(report.Warnings, "deprecated"); n != 1 {
		t.Errorf("expected exactly one deprecation warning, got %d: %v", n, report.Warnings)
	}
}

func TestCheckDisallowedAllowsDeclaredAliases(t *testing.T) {
	var s MigrationSpecification
	os.Clearenv()
	os.Setenv("ENV_CONFIG_OLD_TIMEOUT", "5s")
	os.Setenv("ENV_CONFIG_LEGACY_TIMEOUT", "9s")
	if err := CheckDisallowed("env_config", &s); err != nil {
		t.Errorf("expected declared aliases to be allowed, got %s", err)
	}

	os.Setenv("ENV_CONFIG_UNKNOWN_TIMEOUT", "1s")
	err := CheckDisallowed("env_config", &s)
	if err == nil {
		t.Fatal("expected unknown variable to be reported")
	}
	if experr := "unknown environment variable ENV_CONFIG_UNKNOWN_TIMEOUT"; err.Error() != experr {
		t.Errorf("expected %s, got %s", experr, err)
	}
}

func TestNestedAliasesArePrefixedPerComponent(t *testing.T) {
	var s struct {
		Server struct {
			Timeout string `aliases:"OLD_TIMEOUT"`
		} `envconfig:"SERVER"`
		Client struct {
			Timeout string `aliases:"OLD_TIMEOUT"`
		} `envconfig:"CLIENT"`
	}

	os.Clearenv()
	os.Setenv("APP_SERVER_OLD_TIMEOUT", "server-old")

	report, err := ProcessWithReport("app", &s)
	if err != nil {
		t.Fatal(err)
	}
	if s.Server.Timeout != "server-old" {
		t.Errorf("expected %q, got %q", "server-old", s.Server.Timeout)
	}
	if s.Client.Timeout != "" {
		t.Errorf("client alias must not match server config, got %q", s.Client.Timeout)
	}
	if f := findField(t, report, "Timeout"); f.UsedKey != "APP_SERVER_OLD_TIMEOUT" {
		t.Errorf("expected used key APP_SERVER_OLD_TIMEOUT, got %s", f.UsedKey)
	}

	// The unprefixed OLD_TIMEOUT must not leak into either component.
	os.Clearenv()
	os.Setenv("OLD_TIMEOUT", "unprefixed")
	s.Server.Timeout = ""
	s.Client.Timeout = ""
	if err := Process("app", &s); err != nil {
		t.Fatal(err)
	}
	if s.Server.Timeout != "" || s.Client.Timeout != "" {
		t.Errorf("unprefixed alias must not be used, got server=%q client=%q", s.Server.Timeout, s.Client.Timeout)
	}
}

func TestUsageShowsAliasesAndDeprecation(t *testing.T) {
	var s MigrationSpecification
	os.Clearenv()
	buf := new(bytes.Buffer)
	format := "{{range .}}{{usage_key .}}|{{usage_aliases .}}|{{usage_deprecated .}}\n{{end}}"
	if err := Usagef("env_config", &s, buf, format); err != nil {
		t.Fatal(err)
	}
	expected := "ENV_CONFIG_REQUEST_TIMEOUT|ENV_CONFIG_OLD_TIMEOUT, ENV_CONFIG_LEGACY_TIMEOUT|ENV_CONFIG_OLD_TIMEOUT\n"
	if buf.String() != expected {
		t.Errorf("expected %q, got %q", expected, buf.String())
	}
}
