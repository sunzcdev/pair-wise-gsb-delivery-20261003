package lz4_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/pierrec/lz4/v4"
)

// TestOptimalWriterRoundTrip checks the Optimal level and OptimalOption
// through the frame Writer and CompressingReader, including the memory
// budget degradation path.
func TestOptimalWriterRoundTrip(t *testing.T) {
	phrase := "offline log archival repeated configuration section key=value 0123456789 abcdef ghijkl mnopqr stuvwx yz\n"
	var sb strings.Builder
	for sb.Len() < 600000 {
		sb.WriteString(phrase)
	}
	text := sb.String()
	cases := []struct {
		name string
		opts []lz4.Option
	}{
		{"level", []lz4.Option{lz4.CompressionLevelOption(lz4.Optimal)}},
		{"option-defaults", []lz4.Option{lz4.OptimalOption(lz4.OptimalParams{})}},
		{"option-tiny-budget", []lz4.Option{lz4.OptimalOption(lz4.OptimalParams{MemoryBudget: 1 << 12})}},
		{"option-tuned", []lz4.Option{lz4.OptimalOption(lz4.OptimalParams{Candidates: 16, Gap: 24, Lookahead: 4, ShortMatch: 16})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := lz4.NewWriter(&buf)
			if err := zw.Apply(tc.opts...); err != nil {
				t.Fatal(err)
			}
			if _, err := zw.Write([]byte(text)); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if buf.Len() >= len(text) {
				t.Fatalf("frame did not compress: %d >= %d", buf.Len(), len(text))
			}
			zr := lz4.NewReader(&buf)
			got, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != text {
				t.Fatal("round trip mismatch")
			}
		})
	}
}

// TestOptimalOptionString covers the option stringer and level validation.
func TestOptimalLevelValidation(t *testing.T) {
	if got, want := lz4.Optimal.String(), "Optimal"; got != want {
		t.Fatalf("Optimal.String()=%q want %q", got, want)
	}
	zw := lz4.NewWriter(io.Discard)
	if err := zw.Apply(lz4.CompressionLevelOption(lz4.Optimal)); err != nil {
		t.Fatalf("Optimal level rejected: %v", err)
	}
	if s := lz4.OptimalOption(lz4.OptimalParams{}).String(); !strings.Contains(s, "OptimalOption") {
		t.Fatalf("option string = %q", s)
	}
	// Existing levels are untouched.
	if err := zw.Apply(lz4.CompressionLevelOption(lz4.Level3)); err != nil {
		t.Fatalf("Level3 rejected: %v", err)
	}
}
