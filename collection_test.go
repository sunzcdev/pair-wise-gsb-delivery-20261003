package envconfig

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

type quotedCollectionSpec struct {
	Names        []string              `envconfig:"NAMES" quoted:"true"`
	Numbers      []int                 `envconfig:"NUMBERS" quoted:"true"`
	Brackets     []bracketed           `envconfig:"BRACKETS" quoted:"true"`
	Decoded      []HonorDecodeInStruct `envconfig:"DECODED" quoted:"true"`
	Colors       map[string]int        `envconfig:"COLORS" quoted:"true"`
	EmptyPairs   map[string]string     `envconfig:"EMPTY_PAIRS" quoted:"true"`
	Lists        map[string][]string   `envconfig:"LISTS" quoted:"true"`
	Translations map[string]string     `envconfig:"TRANSLATIONS" quoted:"true"`
	Custom       customCollection      `envconfig:"CUSTOM" quoted:"true"`
}

type customCollection []string

func (c *customCollection) Decode(value string) error {
	*c = customCollection{"decoded:" + value}
	return nil
}

func processQuotedCollectionSpec(t *testing.T, env map[string]string) (*quotedCollectionSpec, error) {
	t.Helper()

	var spec quotedCollectionSpec
	os.Clearenv()
	for key, value := range env {
		os.Setenv(key, value)
	}

	err := Process("", &spec)
	return &spec, err
}

func TestQuotedSliceElements(t *testing.T) {
	spec, err := processQuotedCollectionSpec(t, map[string]string{
		"NAMES":    `"Smith,  Jane"  ,Alice,Hello\"World`,
		"NUMBERS":  `1,"2",3`,
		"BRACKETS": `"a,b",c`,
		"DECODED":  `"a,b",c`,
		"CUSTOM":   `"unterminated`,
	})
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"Smith,  Jane", "Alice", `Hello"World`}; !reflect.DeepEqual(spec.Names, want) {
		t.Errorf("expected %#v, got %#v", want, spec.Names)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(spec.Numbers, want) {
		t.Errorf("expected %#v, got %#v", want, spec.Numbers)
	}
	if want := []bracketed{"[a,b]", "[c]"}; !reflect.DeepEqual(spec.Brackets, want) {
		t.Errorf("expected decoded elements %#v, got %#v", want, spec.Brackets)
	}
	if len(spec.Decoded) != 2 || spec.Decoded[0].Value != "decoded" || spec.Decoded[1].Value != "decoded" {
		t.Errorf("expected Decoder on each element, got %#v", spec.Decoded)
	}
	if want := []string{`decoded:"unterminated`}; !reflect.DeepEqual([]string(spec.Custom), want) {
		t.Errorf("expected top-level Decoder to receive the raw value, got %#v", spec.Custom)
	}
}

func TestQuotedCollectionBoundaries(t *testing.T) {
	spec, err := processQuotedCollectionSpec(t, map[string]string{
		"NAMES":        `""`,
		"NUMBERS":      "",
		"EMPTY_PAIRS":  `"":""`,
		"LISTS":        `first:"a,b",second:"\"c,d\""`,
		"TRANSLATIONS": `" leading key ":" trailing value ",  "outside"  :"  inside  ",untrimmed: kept `,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(spec.Names) != 1 || spec.Names[0] != "" {
		t.Errorf("expected one empty string, got %#v", spec.Names)
	}
	if spec.Numbers == nil || len(spec.Numbers) != 0 {
		t.Errorf("expected empty slice, got %#v", spec.Numbers)
	}
	if len(spec.EmptyPairs) != 1 {
		t.Errorf("expected one map pair, got %#v", spec.EmptyPairs)
	}
	if value := spec.EmptyPairs[""]; value != "" {
		t.Errorf("expected empty key and empty value, got %#v", spec.EmptyPairs)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(spec.Lists["first"], want) {
		t.Errorf("expected nested first list %#v, got %#v", want, spec.Lists["first"])
	}
	if want := []string{"c,d"}; !reflect.DeepEqual(spec.Lists["second"], want) {
		t.Errorf("expected nested quoted list %#v, got %#v", want, spec.Lists["second"])
	}
	if want := " trailing value "; spec.Translations[" leading key "] != want {
		t.Errorf("expected quoted spaces to be preserved, got %#v", spec.Translations)
	}
	if want := "kept"; spec.Translations["untrimmed"] != want {
		t.Errorf("expected unquoted spaces to be trimmed, got %#v", spec.Translations)
	}
	if want := "  inside  "; spec.Translations["outside"] != want {
		t.Errorf("expected spaces around quoted item to be trimmed, got %#v", spec.Translations)
	}
}

func TestQuotedMapQuotedAndEscapedSeparators(t *testing.T) {
	spec, err := processQuotedCollectionSpec(t, map[string]string{
		"COLORS":       `"red,primary":1,"green":2,red:3`,
		"TRANSLATIONS": `morning:"hello: world","key\:with\:escape":"value, with comma"`,
	})
	if err != nil {
		t.Fatal(err)
	}

	if want := map[string]int{"red,primary": 1, "green": 2, "red": 3}; !reflect.DeepEqual(spec.Colors, want) {
		t.Errorf("expected %#v, got %#v", want, spec.Colors)
	}
	if got := spec.Colors["red"]; got != 3 {
		t.Errorf("expected duplicate key to use the last value, got %d", got)
	}
	if want := "hello: world"; spec.Translations["morning"] != want {
		t.Errorf("expected colon in quoted value to be preserved, got %q", spec.Translations["morning"])
	}
	if want := "value, with comma"; spec.Translations["key:with:escape"] != want {
		t.Errorf("expected escaped key to be decoded once, got %#v", spec.Translations)
	}
}

func TestQuotedCollectionSyntaxErrorsReportElement(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "slice unterminated quote",
			env:  map[string]string{"NAMES": `Alice,"Bob`},
			want: "unterminated quote in element 2",
		},
		{
			name: "slice invalid escape",
			env:  map[string]string{"NAMES": `Alice,Bo\x`},
			want: "invalid escape sequence in element 2",
		},
		{
			name: "slice dangling escape",
			env:  map[string]string{"NAMES": `Alice,Bob\`},
			want: "dangling escape in element 2",
		},
		{
			name: "map unterminated quote",
			env:  map[string]string{"COLORS": `red:1,"green:2`},
			want: "unterminated quote in element 2",
		},
		{
			name: "map missing value",
			env:  map[string]string{"COLORS": `red:1,green`},
			want: "invalid map item at element 2",
		},
		{
			name: "map dangling escape",
			env:  map[string]string{"COLORS": `red:1,green:2\`},
			want: "dangling escape in element 2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := processQuotedCollectionSpec(t, test.env)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %q", test.want, err)
			}
		})
	}
}

func TestUnquotedCollectionSyntaxStaysBackwardCompatible(t *testing.T) {
	type spec struct {
		Names []string          `envconfig:"NAMES"`
		Items map[string]string `envconfig:"ITEMS"`
	}

	os.Clearenv()
	os.Setenv("NAMES", ` Alice ,"Bob`)
	os.Setenv("ITEMS", `a:b:c,plain`)

	var got spec
	if err := Process("", &got); err == nil {
		t.Fatal("expected malformed legacy map entry to keep failing")
	}

	os.Clearenv()
	os.Setenv("NAMES", ` Alice ,"Bob`)
	os.Setenv("ITEMS", `a:b,plain:d`)
	if err := Process("", &got); err != nil {
		t.Fatal(err)
	}

	if want := []string{" Alice ", `"Bob`}; !reflect.DeepEqual(got.Names, want) {
		t.Errorf("expected legacy slice parsing %#v, got %#v", want, got.Names)
	}
	if got.Items["a"] != "b" || got.Items["plain"] != "d" {
		t.Errorf("expected legacy map parsing, got %#v", got.Items)
	}
}
