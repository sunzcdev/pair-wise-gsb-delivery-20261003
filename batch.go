package sjson

import (
	jsongo "encoding/json"
	"strconv"
	"unsafe"

	"github.com/tidwall/gjson"
)

// BatchKind identifies the kind of a single batch operation.
type BatchKind int

const (
	// BatchSet sets a json value for the operation path.
	BatchSet BatchKind = iota
	// BatchSetRaw sets a raw json block for the operation path.
	BatchSetRaw
	// BatchDelete deletes the value at the operation path.
	BatchDelete
	// BatchCheck verifies the value at the operation path against the
	// intermediate result produced by the previous operations.
	BatchCheck
)

// BatchOp represents a single operation of a batch update.
// Use SetOp, SetRawOp, DeleteOp, and CheckOp to construct operations.
type BatchOp struct {
	Kind  BatchKind
	Path  string
	Value interface{} // set value, or expected value for BatchCheck
	Raw   string      // raw json block for BatchSetRaw
}

// SetOp returns a batch operation that sets a json value for the path.
func SetOp(path string, value interface{}) BatchOp {
	return BatchOp{Kind: BatchSet, Path: path, Value: value}
}

// SetRawOp returns a batch operation that sets a raw json block for the path.
func SetRawOp(path, value string) BatchOp {
	return BatchOp{Kind: BatchSetRaw, Path: path, Raw: value}
}

// DeleteOp returns a batch operation that deletes the value at the path.
func DeleteOp(path string) BatchOp {
	return BatchOp{Kind: BatchDelete, Path: path}
}

// CheckOp returns a batch operation that verifies the value at the path
// against the intermediate result of the batch. A nil expect requires the
// value to be json null, which is distinct from a missing path: a missing
// path always fails the check.
func CheckOp(path string, expect interface{}) BatchOp {
	return BatchOp{Kind: BatchCheck, Path: path, Value: expect}
}

// BatchError is returned when a batch operation fails. Index is the
// zero-based position of the failed operation.
type BatchError struct {
	Index int
	Op    BatchOp
	Err   error
}

func (err *BatchError) Error() string {
	var kind string
	switch err.Op.Kind {
	case BatchSet:
		kind = "set"
	case BatchSetRaw:
		kind = "setRaw"
	case BatchDelete:
		kind = "delete"
	case BatchCheck:
		kind = "check"
	default:
		kind = "unknown"
	}
	return "sjson: batch operation " + strconv.Itoa(err.Index) +
		" (" + kind + " " + strconv.Quote(err.Op.Path) + ") failed: " +
		err.Err.Error()
}

// Unwrap returns the underlying error of the failed operation.
func (err *BatchError) Unwrap() error {
	return err.Err
}

// valueToRaw converts a value to its raw json representation, the same way
// SetBytesOptions does. The stringify flag reports whether the raw form
// must be wrapped as a json string.
func valueToRaw(value interface{}) (raw string, stringify bool, err error) {
	switch v := value.(type) {
	default:
		b, merr := jsongo.Marshal(value)
		if merr != nil {
			return "", false, merr
		}
		return *(*string)(unsafe.Pointer(&b)), false, nil
	case string:
		return v, true, nil
	case []byte:
		return *(*string)(unsafe.Pointer(&v)), true, nil
	case bool:
		if v {
			return "true", false, nil
		}
		return "false", false, nil
	case int8:
		return strconv.FormatInt(int64(v), 10), false, nil
	case int16:
		return strconv.FormatInt(int64(v), 10), false, nil
	case int32:
		return strconv.FormatInt(int64(v), 10), false, nil
	case int64:
		return strconv.FormatInt(int64(v), 10), false, nil
	case uint8:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case uint16:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case uint64:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 64), false, nil
	case float64:
		return strconv.FormatFloat(float64(v), 'f', -1, 64), false, nil
	}
}

// checkValue verifies the value at path in jstr against expect.
func checkValue(jstr, path string, expect interface{}) error {
	res := gjson.Get(jstr, path)
	if !res.Exists() {
		return &errorType{"check failed: no value at path (missing, not null)"}
	}
	if expect == nil {
		if res.Type != gjson.Null {
			return &errorType{
				"check failed: expected null, got " + res.Raw}
		}
		return nil
	}
	raw, stringify, err := valueToRaw(expect)
	if err != nil {
		return err
	}
	var exp string
	if stringify {
		exp = string(appendStringify(nil, raw))
	} else {
		exp = raw
	}
	if res.Type == gjson.Null {
		return &errorType{"check failed: expected " + exp + ", got null"}
	}
	if res.Raw != exp {
		return &errorType{
			"check failed: expected " + exp + ", got " + res.Raw}
	}
	return nil
}

// Batch applies a series of set, setRaw, delete, and check operations to
// the json, in the given order. Each operation works on the intermediate
// result of the previous operations. If any operation fails, a *BatchError
// is returned and the original json is left unmodified.
func Batch(json string, ops ...BatchOp) (string, error) {
	return BatchOptions(json, ops, nil)
}

// BatchOptions applies a series of batch operations to the json with
// options, in the given order. If any operation fails, a *BatchError is
// returned and the original json is left unmodified.
func BatchOptions(json string, ops []BatchOp, opts *Options) (string, error) {
	if opts != nil {
		if opts.ReplaceInPlace {
			// it's not safe to replace bytes in-place for strings
			// copy the Options and set options.ReplaceInPlace to false.
			nopts := *opts
			opts = &nopts
			opts.ReplaceInPlace = false
		}
	}
	jsonh := *(*stringHeader)(unsafe.Pointer(&json))
	jsonbh := sliceHeader{data: jsonh.data, len: jsonh.len, cap: jsonh.len}
	jsonb := *(*[]byte)(unsafe.Pointer(&jsonbh))
	res, err := BatchBytesOptions(jsonb, ops, opts)
	return string(res), err
}

// BatchBytes applies a series of batch operations to the json, in the
// given order. If working with bytes, this method preferred over
// Batch(string(data), ops...)
func BatchBytes(json []byte, ops ...BatchOp) ([]byte, error) {
	return BatchBytesOptions(json, ops, nil)
}

// BatchBytesOptions applies a series of batch operations to the json with
// options, in the given order. Each operation works on the intermediate
// result of the previous operations, and the result is only delivered when
// all operations succeed. If any operation fails, a *BatchError is returned
// along with the original json; even when Options.ReplaceInPlace is
// enabled, the input bytes are restored to their original state.
func BatchBytesOptions(json []byte, ops []BatchOp,
	opts *Options) ([]byte, error) {
	var inplace bool
	if opts != nil {
		inplace = opts.ReplaceInPlace
	}
	var backup []byte
	if inplace {
		// keep a copy of the original input so it can be restored
		// when any operation fails.
		backup = make([]byte, len(json))
		copy(backup, json)
	}
	cur := json
	for i := range ops {
		op := &ops[i]
		var err error
		switch op.Kind {
		default:
			err = &errorType{"unknown batch operation kind"}
		case BatchSet:
			cur, err = SetBytesOptions(cur, op.Path, op.Value, opts)
		case BatchSetRaw:
			cur, err = SetRawBytesOptions(cur, op.Path,
				[]byte(op.Raw), opts)
		case BatchDelete:
			cur, err = DeleteBytesOptions(cur, op.Path, opts)
		case BatchCheck:
			jstr := *(*string)(unsafe.Pointer(&cur))
			err = checkValue(jstr, op.Path, op.Value)
		}
		if err != nil {
			if inplace {
				// an earlier operation may have modified the
				// input bytes in-place; restore them.
				copy(json, backup)
			}
			return json, &BatchError{Index: i, Op: *op, Err: err}
		}
	}
	return cur, nil
}
