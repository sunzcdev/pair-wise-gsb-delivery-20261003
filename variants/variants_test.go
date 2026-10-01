package variants_test

import (
	"reflect"
	"testing"

	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/variants"
)

type Event interface{ event() }

type Created struct{ ID string }
type Paid struct{ Amount int }
type Unit struct{}

func (*Created) event() {}
func (*Paid) event()    {}
func (*Unit) event()    {}

func TestRegistry(t *testing.T) {
	set, err := variants.New((*Event)(nil)).
		Case("Created", Created{}).
		Case("Paid", &Paid{}). // pointer example is normalized to value
		Case("Unit", Unit{}).
		Build()
	require.NoError(t, err)

	assert.Equal(t, []string{"Created", "Paid", "Unit"}, set.Names())

	v, ok := set.ByName("Paid")
	require.True(t, ok)
	assert.Equal(t, reflect.TypeOf(Paid{}), v.Type)

	// Both value and pointer payloads resolve to the variant.
	byVal, ok := set.ByType(reflect.TypeOf(Paid{}))
	require.True(t, ok)
	assert.Equal(t, "Paid", byVal.Name)
	byPtr, ok := set.ByType(reflect.TypeOf(&Paid{}))
	require.True(t, ok)
	assert.Equal(t, "Paid", byPtr.Name)
}

func TestRegistryMerge(t *testing.T) {
	r := variants.NewRegistry(
		variants.New((*Event)(nil)).Case("Created", Created{}).MustBuild(),
	)
	err := r.Add(variants.New((*Event)(nil)).Case("Paid", Paid{}).MustBuild())
	require.NoError(t, err)

	set, ok := r.SetFor(reflect.TypeOf((*Event)(nil)).Elem())
	require.True(t, ok)
	assert.Equal(t, []string{"Created", "Paid"}, set.Names())

	err = r.Add(variants.New((*Event)(nil)).Case("Created", Unit{}).MustBuild())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate variant name "Created"`)
}

func TestBuildErrors(t *testing.T) {
	_, err := variants.New("not-an-iface").Case("X", Created{}).Build()
	require.Error(t, err)

	_, err = variants.New((*Event)(nil)).Build()
	require.Error(t, err)

	_, err = variants.New((*Event)(nil)).
		Case("A", Created{}).
		Case("A", Paid{}).
		Build()
	require.Error(t, err)

	type other struct{}
	_, err = variants.New((*Event)(nil)).Case("X", other{}).Build()
	require.Error(t, err)
}
