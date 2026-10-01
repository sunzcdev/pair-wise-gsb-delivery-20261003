package zapcore

import "time"

type maskingArrayEncoder struct {
	delegate    ArrayEncoder
	rules       *maskNode
	replacement string
	depth       int
}

func newMaskingArrayEncoder(
	delegate ArrayEncoder,
	rules *maskNode,
	replacement string,
	depth int,
) *maskingArrayEncoder {
	return &maskingArrayEncoder{
		delegate:    delegate,
		rules:       rules,
		replacement: replacement,
		depth:       depth,
	}
}

func (a *maskingArrayEncoder) childArrayRules() *maskNode {
	if a.rules == maskAllNode || a.depth >= maxMaskingDepth {
		return maskAllNode
	}
	return a.rules
}

func (a *maskingArrayEncoder) AppendArray(marshaler ArrayMarshaler) error {
	child := a.childArrayRules()
	if child == nil || !child.hasRules() {
		return a.delegate.AppendArray(marshaler)
	}

	return a.delegate.AppendArray(ArrayMarshalerFunc(func(arr ArrayEncoder) error {
		return runMaskedArray(marshaler, newMaskingArrayEncoder(
			arr, child, a.replacement, a.depth+1,
		))
	}))
}

func (a *maskingArrayEncoder) AppendObject(marshaler ObjectMarshaler) error {
	child := a.childArrayRules()
	if child == nil || !child.hasRules() {
		return a.delegate.AppendObject(marshaler)
	}
	return a.delegate.AppendObject(ObjectMarshalerFunc(func(delegate ObjectEncoder) error {
		return runMaskedObject(marshaler, &maskingObject{
			delegate:    delegate,
			rules:       child,
			replacement: a.replacement,
			depth:       a.depth + 1,
		})
	}))
}

func (a *maskingArrayEncoder) AppendReflected(value interface{}) error {
	child := a.childArrayRules()
	if child == nil || !child.hasRules() {
		return a.delegate.AppendReflected(value)
	}
	if child.terminal {
		a.delegate.AppendString(a.replacement)
		return nil
	}
	return appendMaskedReflected(
		a.delegate, value, child, a.replacement, a.depth+1, nil,
	)
}

func (a *maskingArrayEncoder) AppendBool(value bool) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendBool(value)
}

func (a *maskingArrayEncoder) AppendByteString(value []byte) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendByteString(value)
}

func (a *maskingArrayEncoder) AppendComplex128(value complex128) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendComplex128(value)
}

func (a *maskingArrayEncoder) AppendComplex64(value complex64) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendComplex64(value)
}

func (a *maskingArrayEncoder) AppendFloat64(value float64) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendFloat64(value)
}

func (a *maskingArrayEncoder) AppendFloat32(value float32) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendFloat32(value)
}

func (a *maskingArrayEncoder) AppendInt(value int) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendInt(value)
}

func (a *maskingArrayEncoder) AppendInt64(value int64) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendInt64(value)
}

func (a *maskingArrayEncoder) AppendInt32(value int32) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendInt32(value)
}

func (a *maskingArrayEncoder) AppendInt16(value int16) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendInt16(value)
}

func (a *maskingArrayEncoder) AppendInt8(value int8) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendInt8(value)
}

func (a *maskingArrayEncoder) AppendString(value string) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendString(value)
}

func (a *maskingArrayEncoder) AppendUint(value uint) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendUint(value)
}

func (a *maskingArrayEncoder) AppendUint64(value uint64) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendUint64(value)
}

func (a *maskingArrayEncoder) AppendUint32(value uint32) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendUint32(value)
}

func (a *maskingArrayEncoder) AppendUint16(value uint16) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendUint16(value)
}

func (a *maskingArrayEncoder) AppendUint8(value uint8) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendUint8(value)
}

func (a *maskingArrayEncoder) AppendUintptr(value uintptr) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendUintptr(value)
}

func (a *maskingArrayEncoder) AppendDuration(value time.Duration) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendDuration(value)
}

func (a *maskingArrayEncoder) AppendTime(value time.Time) {
	if a.childArrayRules() != nil {
		a.delegate.AppendString(a.replacement)
		return
	}
	a.delegate.AppendTime(value)
}
