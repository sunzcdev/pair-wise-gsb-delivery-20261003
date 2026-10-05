package mapstructure

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"reflect"
	"sort"
)

// visit identifies a reference already encountered while hashing, so cyclic
// inputs terminate instead of recursing forever.
type visit struct {
	ptr uintptr
	typ reflect.Type
}

// deepDetach returns a value with the same content as v but backed by fresh
// maps, slices and pointers, so later mutation of the original can never
// invalidate the plan and planning can never mutate the caller's data.
func deepDetach(v reflect.Value, seen map[uintptr]bool) reflect.Value {
	if !v.IsValid() {
		return v
	}

	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		ptr := v.Pointer()
		if seen[ptr] {
			return reflect.Zero(v.Type())
		}
		seen[ptr] = true
		nv := reflect.New(v.Type().Elem())
		nv.Elem().Set(deepDetach(v.Elem(), seen))
		delete(seen, ptr)
		return nv

	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		nv := reflect.New(v.Type()).Elem()
		nv.Set(deepDetach(v.Elem(), seen))
		return nv

	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		ptr := v.Pointer()
		if seen[ptr] {
			return reflect.Zero(v.Type())
		}
		seen[ptr] = true
		nv := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			k := deepDetach(iter.Key(), seen)
			e := deepDetach(iter.Value(), seen)
			nv.SetMapIndex(k, e)
		}
		delete(seen, ptr)
		return nv

	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		ptr := v.Pointer()
		if seen[ptr] {
			return reflect.Zero(v.Type())
		}
		seen[ptr] = true
		nv := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			nv.Index(i).Set(deepDetach(v.Index(i), seen))
		}
		delete(seen, ptr)
		return nv

	case reflect.Array:
		nv := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			nv.Index(i).Set(deepDetach(v.Index(i), seen))
		}
		return nv

	case reflect.Struct:
		nv := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if !f.CanSet() {
				// Unexported: decoder cannot touch it either, so leave the
				// copy at its zero value and skip it during the diff.
				continue
			}
			nv.Field(i).Set(deepDetach(f, seen))
		}
		return nv

	default:
		return v
	}
}

// fingerprint hashes the observable structure and content of a value. It is
// used to reject plans whose target or input changed after they were made.
func fingerprint(v reflect.Value, seen map[visit]int) string {
	h := sha256.New()
	fpValue(h, v, seen, 0)
	return hex.EncodeToString(h.Sum(nil))
}

func fpValue(h hash.Hash, v reflect.Value, seen map[visit]int, depth int) {
	if !v.IsValid() {
		writeStr(h, "<invalid>")
		return
	}
	writeStr(h, v.Type().String())

	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			writeStr(h, "<nil>")
			return
		}
		key := visit{v.Pointer(), v.Type()}
		if seen[key] > 0 {
			writeStr(h, "<cycle>")
			return
		}
		seen[key]++
		fpValue(h, v.Elem(), seen, depth+1)
		seen[key]--

	case reflect.Interface:
		if v.IsNil() {
			writeStr(h, "<nil>")
			return
		}
		fpValue(h, v.Elem(), seen, depth+1)

	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			writeStr(h, v.Type().Field(i).Name)
			fpValue(h, v.Field(i), seen, depth+1)
		}

	case reflect.Map:
		if v.IsNil() {
			writeStr(h, "<nil>")
			return
		}
		key := visit{v.Pointer(), v.Type()}
		if seen[key] > 0 {
			writeStr(h, "<cycle>")
			return
		}
		seen[key]++
		writeInt(h, int64(v.Len()))
		keys := v.MapKeys()
		type pair struct {
			fp string
			k  reflect.Value
		}
		pairs := make([]pair, len(keys))
		for i, k := range keys {
			kh := sha256.New()
			fpValue(kh, k, map[visit]int{}, 0)
			pairs[i] = pair{fp: hex.EncodeToString(kh.Sum(nil)), k: k}
		}
		sort.Slice(pairs, func(a, b int) bool { return pairs[a].fp < pairs[b].fp })
		for _, pr := range pairs {
			writeStr(h, pr.fp)
			fpValue(h, v.MapIndex(pr.k), seen, depth+1)
		}
		seen[key]--

	case reflect.Slice, reflect.Array:
		writeInt(h, int64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			fpValue(h, v.Index(i), seen, depth+1)
		}

	case reflect.String:
		writeStr(h, v.String())

	case reflect.Bool:
		if v.Bool() {
			writeStr(h, "1")
		} else {
			writeStr(h, "0")
		}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		writeInt(h, v.Int())

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		writeUint(h, v.Uint())

	case reflect.Float32, reflect.Float64:
		bits := make([]byte, 8)
		binary.LittleEndian.PutUint64(bits, uint64(v.Float()))
		_, _ = h.Write(bits)

	case reflect.Complex64, reflect.Complex128:
		writeStr(h, fmt.Sprintf("%v", v.Complex()))

	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		writeUint(h, uint64(v.Pointer()))

	default:
		if v.CanInterface() {
			writeStr(h, "<nil>")
		} else {
			writeStr(h, "<uninterfaced>")
		}
	}
}

func configFingerprint(c *DecoderConfig) string {
	h := sha256.New()
	writeBool := func(b bool) {
		if b {
			writeStr(h, "1")
		} else {
			writeStr(h, "0")
		}
	}
	writePtr := func(f any) {
		v := reflect.ValueOf(f)
		if !v.IsValid() || (v.Kind() == reflect.Ptr && v.IsNil()) {
			writeUint(h, 0)
			return
		}
		writeUint(h, uint64(v.Pointer()))
	}

	writePtr(c.DecodeHook)
	writeBool(c.ErrorUnused)
	writeBool(c.ErrorUnset)
	writeBool(c.AllowUnsetPointer)
	writeBool(c.ZeroFields)
	writeBool(c.WeaklyTypedInput)
	writeBool(c.Squash)
	writeBool(c.Deep)
	writeStr(h, c.TagName)
	writeStr(h, c.RootName)
	writeStr(h, c.SquashTagOption)
	writeBool(c.IgnoreUntaggedFields)
	writePtr(c.MatchName)
	writeBool(c.DecodeNil)
	writePtr(c.MapFieldName)
	writeBool(c.DisableUnmarshaler)

	return hex.EncodeToString(h.Sum(nil))
}

func writeStr(h hash.Hash, s string) {
	writeInt(h, int64(len(s)))
	_, _ = h.Write([]byte(s))
}

func writeInt(h hash.Hash, n int64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(n))
	_, _ = h.Write(buf[:])
}

func writeUint(h hash.Hash, n uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(n))
	_, _ = h.Write(buf[:])
}
