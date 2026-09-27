package validator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	. "github.com/go-playground/assert/v2"
)

// fakeExistsBackend is a fake remote "exists" service counting every call it receives.
type fakeExistsBackend struct {
	mu       sync.Mutex
	calls    []recordedCall
	existing map[string]map[string]bool // scope -> key -> exists
	err      error
	onCall   func(scope string)
}

type recordedCall struct {
	scope string
	keys  []string
}

func (f *fakeExistsBackend) check(ctx context.Context, scope string, keys []string) (map[string]bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{scope: scope, keys: append([]string(nil), keys...)})
	f.mu.Unlock()

	if f.onCall != nil {
		f.onCall(scope)
	}

	if f.err != nil {
		return nil, f.err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	res := make(map[string]bool, len(keys))
	for _, k := range keys {
		res[k] = f.existing[scope][k]
	}
	return res, nil
}

func (f *fakeExistsBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeExistsBackend) call(idx int) recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[idx]
}

func sortedKeys(keys []string) []string {
	out := append([]string(nil), keys...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

type batchLineItem struct {
	SKU string `validate:"required,sku_exists"`
}

type batchOrderRequest struct {
	Items []batchLineItem `validate:"dive"`
}

func TestBatchValidationDedupAndBackfill(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: map[string]map[string]bool{
			"": {"SKU-1": true, "SKU-2": true, "SKU-3": true},
		},
	}

	validate := New()
	err := validate.RegisterBatchValidation("sku_exists", backend.check)
	Equal(t, err, nil)

	// one request, 120 line items, only 4 distinct SKUs, one of them unknown
	skus := []string{"SKU-1", "SKU-2", "SKU-3", "SKU-GONE"}
	req := batchOrderRequest{Items: make([]batchLineItem, 120)}
	for i := range req.Items {
		req.Items[i].SKU = skus[i%len(skus)]
	}

	err = validate.Struct(req)

	// dedup: a single backend call for the whole request, with 4 unique keys
	Equal(t, backend.callCount(), 1)
	Equal(t, sortedKeys(backend.call(0).keys), []string{"SKU-1", "SKU-2", "SKU-3", "SKU-GONE"})

	// backfill: exactly the 30 fields holding SKU-GONE are reported, in place
	ve, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(ve), 30)
	for _, fe := range ve {
		Equal(t, fe.Tag(), "sku_exists")
		Equal(t, fe.Field(), "SKU")
		Equal(t, fe.Value(), "SKU-GONE")
		Equal(t, strings.HasPrefix(fe.Namespace(), "batchOrderRequest.Items["), true)
		Equal(t, strings.HasSuffix(fe.Namespace(), "].SKU"), true)
	}
}

type tenantOrder struct {
	Tenant string
	SKUs   []string `validate:"dive,sku_exists"`
}

type tenantBatchRequest struct {
	Orders []tenantOrder `validate:"dive"`
}

func tenantScopeFn(ctx context.Context, fl FieldLevel) string {
	return fl.Parent().FieldByName("Tenant").String()
}

func TestBatchValidationTenantIsolation(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: map[string]map[string]bool{
			"acme":   {"SKU-A": true, "SKU-B": true, "SKU-C": true},
			"globex": {"SKU-A": true},
		},
	}

	validate := New()
	err := validate.RegisterBatchValidation("sku_exists", backend.check, WithBatchScope(tenantScopeFn))
	Equal(t, err, nil)

	req := tenantBatchRequest{Orders: []tenantOrder{
		{Tenant: "acme", SKUs: []string{"SKU-A", "SKU-B", "SKU-B", "SKU-C"}},
		{Tenant: "globex", SKUs: []string{"SKU-A", "SKU-B"}},
	}}

	err = validate.Struct(req)

	// same keys, different tenants: two separate calls, nothing mixed
	Equal(t, backend.callCount(), 2)

	calls := map[string][]string{}
	for i := 0; i < 2; i++ {
		c := backend.call(i)
		calls[c.scope] = c.keys
	}
	Equal(t, sortedKeys(calls["acme"]), []string{"SKU-A", "SKU-B", "SKU-C"})
	Equal(t, sortedKeys(calls["globex"]), []string{"SKU-A", "SKU-B"})

	// SKU-B exists for acme but not for globex: exactly one error, on globex's field
	ve, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(ve), 1)
	Equal(t, ve[0].Tag(), "sku_exists")
	Equal(t, ve[0].Value(), "SKU-B")
	Equal(t, ve[0].Namespace(), "tenantBatchRequest.Orders[1].SKUs[1]")
}

func TestBatchValidationBackendUnavailable(t *testing.T) {
	backendErr := errors.New("dial tcp: connection refused")
	backend := &fakeExistsBackend{err: backendErr}

	validate := New()
	err := validate.RegisterBatchValidation("sku_exists", backend.check)
	Equal(t, err, nil)

	err = validate.Struct(batchOrderRequest{Items: []batchLineItem{{SKU: "SKU-1"}}})

	// backend down is NOT a "product does not exist" validation failure
	_, isValidationErrs := err.(ValidationErrors)
	Equal(t, isValidationErrs, false)

	var be *BatchError
	Equal(t, errors.As(err, &be), true)
	Equal(t, be.Tag, "sku_exists")
	Equal(t, errors.Is(err, backendErr), true)
}

func TestBatchValidationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := &fakeExistsBackend{
		existing: map[string]map[string]bool{
			"acme":   {"SKU-A": true},
			"globex": {"SKU-A": true},
		},
		// the first backend call cancels the context mid-validation
		onCall: func(scope string) { cancel() },
	}

	validate := New()
	err := validate.RegisterBatchValidation("sku_exists", backend.check, WithBatchScope(tenantScopeFn))
	Equal(t, err, nil)

	req := tenantBatchRequest{Orders: []tenantOrder{
		{Tenant: "acme", SKUs: []string{"SKU-A"}},
		{Tenant: "globex", SKUs: []string{"SKU-A"}},
	}}

	err = validate.StructCtx(ctx, req)

	// cancellation stops resolution: the second tenant group is never fetched
	Equal(t, backend.callCount(), 1)

	_, isValidationErrs := err.(ValidationErrors)
	Equal(t, isValidationErrs, false)

	var be *BatchError
	Equal(t, errors.As(err, &be), true)
	Equal(t, errors.Is(err, context.Canceled), true)
}

func TestBatchValidationLocalRulesUnaffected(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: map[string]map[string]bool{
			"": {"SKU-VALID": true},
		},
	}

	validate := New()
	err := validate.RegisterBatchValidation("sku_exists", backend.check)
	Equal(t, err, nil)

	type localFirst struct {
		Empty string `validate:"required,sku_exists"`
		Short string `validate:"omitempty,min=10,sku_exists"`
		OK    string `validate:"required,sku_exists"`
	}

	err = validate.Struct(localFirst{Empty: "", Short: "short", OK: "SKU-VALID"})

	// format validations still run locally; failed local checks emit no keys
	Equal(t, backend.callCount(), 1)
	Equal(t, backend.call(0).keys, []string{"SKU-VALID"})

	ve, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(ve), 2)

	tags := map[string]string{}
	for _, fe := range ve {
		tags[fe.Field()] = fe.Tag()
	}
	Equal(t, tags["Empty"], "required")
	Equal(t, tags["Short"], "min")
}

func TestBatchValidationMissingKeyFailsClosed(t *testing.T) {
	// backend answers but omits a key from the result map
	backend := &fakeExistsBackend{existing: map[string]map[string]bool{}}

	validate := New()
	err := validate.RegisterBatchValidation("sku_exists", backend.check)
	Equal(t, err, nil)

	err = validate.Struct(batchOrderRequest{Items: []batchLineItem{{SKU: "SKU-UNKNOWN"}}})

	ve, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(ve), 1)
	Equal(t, ve[0].Tag(), "sku_exists")
}

func TestBatchValidationRegistration(t *testing.T) {
	validate := New()

	err := validate.RegisterBatchValidation("", func(ctx context.Context, scope string, keys []string) (map[string]bool, error) {
		return nil, nil
	})
	NotEqual(t, err, nil)

	err = validate.RegisterBatchValidation("sku_exists", nil)
	NotEqual(t, err, nil)

	// batch tags are not supported inside or-expressions
	err = validate.RegisterBatchValidation("sku_exists", func(ctx context.Context, scope string, keys []string) (map[string]bool, error) {
		return map[string]bool{}, nil
	})
	Equal(t, err, nil)

	type orMisuse struct {
		SKU string `validate:"sku_exists|min=1"`
	}
	PanicMatches(t, func() { _ = validate.Struct(orMisuse{SKU: "x"}) }, "batch validation 'sku_exists' cannot be used within an or (|) expression")
}

func TestBatchValidationCustomKeyFunc(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: map[string]map[string]bool{
			"": {"SKU-1": true},
		},
	}

	validate := New()
	err := validate.RegisterBatchValidation("sku_exists", backend.check,
		WithBatchKey(func(ctx context.Context, fl FieldLevel) string {
			return fmt.Sprintf("SKU-%s", fl.Field().String())
		}),
	)
	Equal(t, err, nil)

	type req struct {
		ID string `validate:"sku_exists"`
	}

	Equal(t, validate.Struct(req{ID: "1"}), nil)
	Equal(t, backend.callCount(), 1)
	Equal(t, backend.call(0).keys, []string{"SKU-1"})
}
