// Copyright (c) 2026. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package envconfig

import (
	"bytes"
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

type aliasSpec struct {
	RequestTimeout string `envconfig:"REQUEST_TIMEOUT" aliases:"OLD_TIMEOUT,LEGACY_TIMEOUT" deprecated:"renamed to REQUEST_TIMEOUT"`
	Plain          string `envconfig:"PLAIN"`
}

func captureLog(fn func()) string {
	var buf bytes.Buffer
	old := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(old)
		log.SetFlags(oldFlags)
	}()
	fn()
	return buf.String()
}

func TestAliasesLoadFromOldName(t *testing.T) {
	var s aliasSpec
	os.Clearenv()
	os.Setenv("APP_OLD_TIMEOUT", "30s")

	report, err := ProcessWithReport("app", &s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.RequestTimeout != "30s" {
		t.Errorf("expected %q, got %q", "30s", s.RequestTimeout)
	}
	if len(report.Fields) != 2 {
		t.Fatalf("expected 2 field reports, got %d", len(report.Fields))
	}
	fl := report.Fields[0]
	if fl.Field != "RequestTimeout" {
		t.Errorf("unexpected field name %q", fl.Field)
	}
	if fl.Key != "APP_REQUEST_TIMEOUT" {
		t.Errorf("unexpected canonical key %q", fl.Key)
	}
	if fl.Source != "APP_OLD_TIMEOUT" {
		t.Errorf("expected source APP_OLD_TIMEOUT, got %q", fl.Source)
	}
	if len(fl.Aliases) != 2 || fl.Aliases[0] != "APP_OLD_TIMEOUT" ||
		fl.Aliases[1] != "APP_LEGACY_TIMEOUT" {
		t.Errorf("unexpected aliases %#v", fl.Aliases)
	}
	if !fl.Deprecated {
		t.Error("expected field to be marked deprecated")
	}
	if len(fl.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %#v", fl.Warnings)
	}
	if !strings.Contains(fl.Warnings[0], "APP_OLD_TIMEOUT is deprecated") ||
		!strings.Contains(fl.Warnings[0], "use APP_REQUEST_TIMEOUT instead") ||
		!strings.Contains(fl.Warnings[0], "renamed to REQUEST_TIMEOUT") {
		t.Errorf("unexpected warning: %q", fl.Warnings[0])
	}
	if len(report.Warnings) != 1 || report.Warnings[0] != fl.Warnings[0] {
		t.Errorf("unexpected aggregated warnings: %#v", report.Warnings)
	}
}

func TestAliasesCanonicalWins(t *testing.T) {
	var s aliasSpec
	os.Clearenv()
	os.Setenv("APP_REQUEST_TIMEOUT", "5m")
	os.Setenv("APP_OLD_TIMEOUT", "30s")

	logOut := captureLog(func() {
		// Default policy treats differing values as a configuration error.
		if _, err := ProcessWithReport("app", &s); err == nil {
			t.Fatal("expected a conflict error with the default policy")
		}
		// With the priority policy the canonical name takes precedence silently.
		report, err := ProcessConfig("app", &s, Config{ConflictPolicy: ConflictPriority})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if report.Fields[0].Source != "APP_REQUEST_TIMEOUT" {
			t.Errorf("unexpected source %q", report.Fields[0].Source)
		}
	})
	if s.RequestTimeout != "5m" {
		t.Errorf("expected canonical value %q, got %q", "5m", s.RequestTimeout)
	}
	if strings.Contains(logOut, "deprecated") {
		t.Errorf("no deprecation warning when canonical name is used, got %q", logOut)
	}
}

func TestAliasesConflictingValuesError(t *testing.T) {
	var s aliasSpec
	os.Clearenv()
	os.Setenv("APP_OLD_TIMEOUT", "30s")
	os.Setenv("APP_LEGACY_TIMEOUT", "60s")

	_, err := ProcessWithReport("app", &s)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "conflicting environment variables") ||
		!strings.Contains(err.Error(), "APP_OLD_TIMEOUT") ||
		!strings.Contains(err.Error(), "APP_LEGACY_TIMEOUT") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAliasesConflictingValuesPriority(t *testing.T) {
	var s struct {
		Timeout string `aliases:"OLD_TIMEOUT" on_conflict:"priority"`
	}
	os.Clearenv()
	os.Setenv("APP_TIMEOUT", "5m")
	os.Setenv("APP_OLD_TIMEOUT", "30s")

	report, err := ProcessWithReport("app", &s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Timeout != "5m" {
		t.Errorf("expected canonical value %q, got %q", "5m", s.Timeout)
	}
	if len(report.Warnings) != 1 ||
		!strings.Contains(report.Warnings[0], "using APP_TIMEOUT") {
		t.Errorf("expected one priority warning, got %#v", report.Warnings)
	}

	// Alias order is the precedence when canonical is absent.
	os.Clearenv()
	os.Setenv("APP_OLD_TIMEOUT", "30s")
	os.Setenv("APP_TIMEOUT", "9m")
	report2, err2 := ProcessConfig("app", &s, Config{ConflictPolicy: ConflictError})
	if err2 != nil {
		t.Fatalf("field tag should override config, got %v", err2)
	}
	if s.Timeout != "9m" {
		t.Errorf("expected canonical value %q, got %q", "9m", s.Timeout)
	}
	if report2.Fields[0].Source != "APP_TIMEOUT" {
		t.Errorf("unexpected source %q", report2.Fields[0].Source)
	}
}

func TestAliasesSameValueSingleWarning(t *testing.T) {
	var s aliasSpec
	os.Clearenv()
	os.Setenv("APP_OLD_TIMEOUT", "30s")
	os.Setenv("APP_LEGACY_TIMEOUT", "30s")

	report, err := ProcessWithReport("app", &s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.RequestTimeout != "30s" {
		t.Errorf("expected %q, got %q", "30s", s.RequestTimeout)
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("expected exactly one deprecation warning, got %#v", report.Warnings)
	}
	if !strings.Contains(report.Warnings[0], "APP_OLD_TIMEOUT, APP_LEGACY_TIMEOUT") {
		t.Errorf("warning should mention both old names: %q", report.Warnings[0])
	}

	// Plain Process also logs the single warning.
	os.Clearenv()
	os.Setenv("APP_OLD_TIMEOUT", "30s")
	os.Setenv("APP_LEGACY_TIMEOUT", "30s")
	logOut := captureLog(func() {
		if err := Process("app", &s); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if strings.Count(logOut, "deprecated") != 1 {
		t.Errorf("expected one log line, got %q", logOut)
	}
}

func TestCheckDisallowedAcceptsDeclaredAliases(t *testing.T) {
	var s aliasSpec
	os.Clearenv()
	os.Setenv("APP_OLD_TIMEOUT", "30s")
	os.Setenv("APP_LEGACY_TIMEOUT", "60s")
	os.Setenv("APP_PLAIN", "x")

	if err := CheckDisallowed("app", &s); err != nil {
		t.Errorf("declared aliases must be allowed, got %v", err)
	}

	os.Setenv("APP_TYPO_TIMEOUT", "1s")
	err := CheckDisallowed("app", &s)
	if err == nil || err.Error() != "unknown environment variable APP_TYPO_TIMEOUT" {
		t.Errorf("expected unknown variable error, got %v", err)
	}
}

func TestAliasesPrefixedAndNestedDoNotCrossComponents(t *testing.T) {
	type component struct {
		Timeout string `aliases:"OLD_TIMEOUT"`
	}
	type root struct {
		HTTP component
		RPC  component
	}
	var s root
	os.Clearenv()

	// HTTP's legacy name must not populate RPC's field.
	os.Setenv("APP_HTTP_OLD_TIMEOUT", "2s")
	os.Setenv("APP_RPC_TIMEOUT", "9s")

	if err := Process("app", &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.HTTP.Timeout != "2s" {
		t.Errorf("expected http timeout 2s, got %q", s.HTTP.Timeout)
	}
	if s.RPC.Timeout != "9s" {
		t.Errorf("expected rpc timeout 9s, got %q", s.RPC.Timeout)
	}

	// The sibling component's alias is an unknown key for this component.
	if err := CheckDisallowed("app_http", &s); err == nil ||
		!strings.Contains(err.Error(), "unknown environment variable") {
		t.Errorf("expected unknown variable error, got %v", err)
	}
}

func TestMixedDeprecatedAliases(t *testing.T) {
	var s struct {
		Value string `aliases:"OLD_VALUE,CURRENT_ALIAS" deprecated_aliases:"OLD_VALUE"`
	}
	os.Clearenv()
	os.Setenv("APP_CURRENT_ALIAS", "new")
	os.Setenv("APP_OLD_VALUE", "new")

	logOut := captureLog(func() {
		report, err := ProcessWithReport("app", &s)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.Value != "new" {
			t.Errorf("expected %q, got %q", "new", s.Value)
		}
		if len(report.Warnings) != 1 {
			t.Fatalf("expected one deprecation warning only for OLD_VALUE, got %#v", report.Warnings)
		}
		if !strings.Contains(report.Warnings[0], "APP_OLD_VALUE") ||
			strings.Contains(report.Warnings[0], "CURRENT_ALIAS") {
			t.Errorf("unexpected warning: %q", report.Warnings[0])
		}
	})
	if strings.Count(logOut, "deprecated") != 1 {
		t.Errorf("expected one log line, got %q", logOut)
	}
}

func TestAliasesWithoutDeprecationStaySilent(t *testing.T) {
	var s struct {
		Value string `aliases:"ALTERNATIVE"`
	}
	os.Clearenv()
	os.Setenv("APP_ALTERNATIVE", "x")

	report, err := ProcessWithReport("app", &s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Value != "x" {
		t.Errorf("expected %q, got %q", "x", s.Value)
	}
	if report.Fields[0].Deprecated {
		t.Error("field should not be marked deprecated")
	}
	if len(report.Warnings) != 0 {
		t.Errorf("expected no warnings, got %#v", report.Warnings)
	}
}

func TestUsageShowsMigrationRelationship(t *testing.T) {
	var s aliasSpec
	os.Clearenv()
	buf := new(bytes.Buffer)
	if err := Usagef("app", &s, buf,
		"{{range .}}{{usage_key .}} -> {{usage_aliases .}} [deprecated={{usage_deprecated .}}]{{end}}"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "APP_REQUEST_TIMEOUT -> APP_OLD_TIMEOUT,APP_LEGACY_TIMEOUT") {
		t.Errorf("usage missing migration mapping: %q", got)
	}
	if !strings.Contains(got, "deprecated=true (renamed to REQUEST_TIMEOUT)") {
		t.Errorf("usage missing deprecation marker: %q", got)
	}
	if !strings.Contains(got, "APP_PLAIN ->  [deprecated=]") {
		t.Errorf("usage missing plain field row: %q", got)
	}
}

func TestDefaultTableUsageContainsAliasesColumn(t *testing.T) {
	var s aliasSpec
	os.Clearenv()
	buf := new(bytes.Buffer)
	if err := Usagef("app", &s, buf, DefaultTableFormat); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(buf)
	if !strings.Contains(string(out), "ALIASES") ||
		!strings.Contains(string(out), "APP_OLD_TIMEOUT,APP_LEGACY_TIMEOUT") {
		t.Errorf("table usage missing alias column/data:\n%s", out)
	}
}
