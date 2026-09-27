package validator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/go-playground/assert/v2"
)

// fakeExistsBackend is a fake remote "does it exist" service counting its calls.
type fakeExistsBackend struct {
	mu       sync.Mutex
	calls    int
	gotKeys  [][]string
	existing func(ctx context.Context, key string) bool
	err      error
	onCall   func(ctx context.Context) error
}

func (f *fakeExistsBackend) check(ctx context.Context, keys []string) (map[string]bool, error) {
	f.mu.Lock()
	f.calls++
	f.gotKeys = append(f.gotKeys, append([]string(nil), keys...))
	f.mu.Unlock()

	if f.onCall != nil {
		if err := f.onCall(ctx); err != nil {
			return nil, err
		}
	}

	if f.err != nil {
		return nil, f.err
	}

	valid := make(map[string]bool, len(keys))
	for _, k := range keys {
		valid[k] = f.existing(ctx, k)
	}
	return valid, nil
}

func (f *fakeExistsBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeExistsBackend) keysOf(call int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotKeys[call]...)
}

type tenantCtxKey struct{}

func tenantFromCtx(ctx context.Context) string {
	s, _ := ctx.Value(tenantCtxKey{}).(string)
	return s
}

type batchOrderLine struct {
	SKU string `validate:"required,sku_exists"`
}

type batchOrder struct {
	Lines []batchOrderLine `validate:"dive"`
}

func newSKUValidator(t *testing.T, backend *fakeExistsBackend, scoped bool) *Validate {
	t.Helper()

	v := New()
	rule := BatchExternalRule{
		Key: func(fl FieldLevel) (string, bool) {
			s, ok := fl.Field().Interface().(string)
			return s, ok && s != ""
		},
		Check: backend.check,
	}
	if scoped {
		rule.Scope = tenantFromCtx
	}
	if err := v.RegisterBatchExternal("sku_exists", rule); err != nil {
		t.Fatalf("RegisterBatchExternal failed: %v", err)
	}
	return v
}

func TestBatchExternalDedupAndBackfill(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: func(_ context.Context, key string) bool { return key != "MISSING" },
	}
	v := newSKUValidator(t, backend, false)

	// one request with hundreds of line items, only 5 distinct SKUs
	skus := []string{"SKU-1", "SKU-2", "SKU-3", "SKU-4", "MISSING"}
	order := batchOrder{Lines: make([]batchOrderLine, 120)}
	for i := range order.Lines {
		order.Lines[i].SKU = skus[i%len(skus)]
	}

	err := v.StructBatch(order)
	NotEqual(t, err, nil)

	// deduplicated: a single backend call with the 5 unique keys
	Equal(t, backend.callCount(), 1)
	Equal(t, len(backend.keysOf(0)), len(skus))

	// mapped back: one error per line holding the missing SKU
	ve, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(ve), 120/len(skus))

	for i, fe := range ve {
		Equal(t, fe.Tag(), "sku_exists")
		Equal(t, fe.Value(), "MISSING")
		Equal(t, fe.Namespace(), fmt.Sprintf("batchOrder.Lines[%d].SKU", (i+1)*len(skus)-1))
	}
}

func TestBatchExternalLocalValidationStillRuns(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: func(_ context.Context, key string) bool { return true },
	}
	v := newSKUValidator(t, backend, false)

	order := batchOrder{Lines: []batchOrderLine{{SKU: "SKU-1"}, {SKU: ""}}}

	err := v.StructBatch(order)
	NotEqual(t, err, nil)

	// local format validation ("required") fails locally
	ve, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(ve), 1)
	Equal(t, ve[0].Tag(), "required")
	Equal(t, ve[0].Namespace(), "batchOrder.Lines[1].SKU")

	// the empty value was never sent to the backend
	Equal(t, backend.callCount(), 1)
	Equal(t, len(backend.keysOf(0)), 1)
	Equal(t, backend.keysOf(0)[0], "SKU-1")
}

func TestBatchExternalBackendUnavailable(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: func(_ context.Context, key string) bool { return true },
		err:      errors.New("connection refused"),
	}
	v := newSKUValidator(t, backend, false)

	order := batchOrder{Lines: []batchOrderLine{{SKU: "SKU-1"}}}

	err := v.StructBatch(order)
	NotEqual(t, err, nil)

	// backend unavailable is NOT a "does not exist" validation failure
	_, isValidationErrs := err.(ValidationErrors)
	Equal(t, isValidationErrs, false)

	var be *BatchExternalError
	Equal(t, errors.As(err, &be), true)
	Equal(t, be.Tag, "sku_exists")
	Equal(t, be.Err.Error(), "connection refused")
}

func TestBatchExternalTenantIsolation(t *testing.T) {
	// SKU-1 exists for tenant "a" only
	backend := &fakeExistsBackend{
		existing: func(ctx context.Context, key string) bool {
			return tenantFromCtx(ctx) == "a" && key == "SKU-1"
		},
	}
	v := newSKUValidator(t, backend, true)

	order := batchOrder{Lines: []batchOrderLine{{SKU: "SKU-1"}}}

	ctxA := context.WithValue(context.Background(), tenantCtxKey{}, "a")
	ctxB := context.WithValue(context.Background(), tenantCtxKey{}, "b")

	err := v.StructBatchCtx(ctxA, order)
	Equal(t, err, nil)

	err = v.StructBatchCtx(ctxB, order)
	NotEqual(t, err, nil)

	ve, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(ve), 1)
	Equal(t, ve[0].Tag(), "sku_exists")

	// identical keys of different tenants are never merged:
	// one backend call per tenant, each with the full key set
	Equal(t, backend.callCount(), 2)
	Equal(t, len(backend.keysOf(0)), 1)
	Equal(t, len(backend.keysOf(1)), 1)
	Equal(t, backend.keysOf(0)[0], "SKU-1")
	Equal(t, backend.keysOf(1)[0], "SKU-1")
}

func TestBatchExternalCancellation(t *testing.T) {
	t.Run("before call", func(t *testing.T) {
		backend := &fakeExistsBackend{
			existing: func(_ context.Context, key string) bool { return true },
		}
		v := newSKUValidator(t, backend, false)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := v.StructBatchCtx(ctx, batchOrder{Lines: []batchOrderLine{{SKU: "SKU-1"}}})
		NotEqual(t, err, nil)
		Equal(t, errors.Is(err, context.Canceled), true)

		// the backend was never called
		Equal(t, backend.callCount(), 0)
	})

	t.Run("mid flight", func(t *testing.T) {
		backend := &fakeExistsBackend{
			existing: func(_ context.Context, key string) bool { return true },
			onCall: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
		v := newSKUValidator(t, backend, false)

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		defer cancel()

		err := v.StructBatchCtx(ctx, batchOrder{Lines: []batchOrderLine{{SKU: "SKU-1"}}})
		NotEqual(t, err, nil)

		// cancellation is surfaced as the context error, not as field errors
		Equal(t, errors.Is(err, context.Canceled), true)
		_, isValidationErrs := err.(ValidationErrors)
		Equal(t, isValidationErrs, false)
		Equal(t, backend.callCount(), 1)
	})
}

func TestBatchExternalSkippedWithoutBatchEntryPoint(t *testing.T) {
	backend := &fakeExistsBackend{
		existing: func(_ context.Context, key string) bool { return false },
	}
	v := newSKUValidator(t, backend, false)

	// regular entry points run local validations only, no backend call
	err := v.Struct(batchOrder{Lines: []batchOrderLine{{SKU: "SKU-1"}}})
	Equal(t, err, nil)
	Equal(t, backend.callCount(), 0)
}

func TestBatchExternalRegistration(t *testing.T) {
	v := New()

	err := v.RegisterBatchExternal("no_key", BatchExternalRule{
		Check: func(context.Context, []string) (map[string]bool, error) { return nil, nil },
	})
	NotEqual(t, err, nil)

	err = v.RegisterBatchExternal("no_check", BatchExternalRule{
		Key: func(fl FieldLevel) (string, bool) { return "", false },
	})
	NotEqual(t, err, nil)

	err = v.RegisterBatchExternal("ok", BatchExternalRule{
		Key:   func(fl FieldLevel) (string, bool) { return "", false },
		Check: func(context.Context, []string) (map[string]bool, error) { return nil, nil },
	})
	Equal(t, err, nil)

	// invalid input still reported as InvalidValidationError
	err = v.StructBatch("not a struct")
	NotEqual(t, err, nil)
	_, ok := err.(*InvalidValidationError)
	Equal(t, ok, true)
}
