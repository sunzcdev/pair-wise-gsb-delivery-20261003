package mysql

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrQueryAttributesNotSupported is returned when query attributes are
// requested on a connection that did not negotiate CLIENT_QUERY_ATTRIBUTES.
var ErrQueryAttributesNotSupported = errors.New("mysql: server does not support query attributes")

// QueryAttribute is a request-scoped MySQL query attribute. It is not encoded
// as an ordinary prepared-statement parameter and is not substituted into the
// SQL text.
type QueryAttribute struct {
	Name  string
	Value any
}

type queryAttributesContextKey struct{}

type queryAttributesOptions struct {
	attributes     []QueryAttribute
	ignoreIfAbsent bool
}

// WithQueryAttributes attaches request-scoped MySQL query attributes. The
// attributes are sent once with the next text query or prepared-statement
// execution and are not stored on the connection. If the server does not
// support CLIENT_QUERY_ATTRIBUTES, the request fails with
// [ErrQueryAttributesNotSupported].
func WithQueryAttributes(ctx context.Context, attrs ...QueryAttribute) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	copied := make([]QueryAttribute, len(attrs))
	copy(copied, attrs)
	return context.WithValue(ctx, queryAttributesContextKey{}, queryAttributesOptions{attributes: copied})
}

// WithQueryAttributesIfSupported behaves like [WithQueryAttributes], except
// that the request proceeds without query attributes when the connected server
// lacks CLIENT_QUERY_ATTRIBUTES support.
func WithQueryAttributesIfSupported(ctx context.Context, attrs ...QueryAttribute) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	copied := make([]QueryAttribute, len(attrs))
	copy(copied, attrs)
	return context.WithValue(ctx, queryAttributesContextKey{}, queryAttributesOptions{
		attributes:     copied,
		ignoreIfAbsent: true,
	})
}

func queryAttributesFromContext(ctx context.Context) ([]QueryAttribute, bool) {
	options, ok := ctx.Value(queryAttributesContextKey{}).(queryAttributesOptions)
	if !ok {
		return nil, false
	}
	return options.attributes, options.ignoreIfAbsent
}

func (mc *mysqlConn) prepareQueryAttributes(ctx context.Context) ([]driver.Value, []string, error) {
	attrs, ignoreIfAbsent := queryAttributesFromContext(ctx)
	if len(attrs) == 0 {
		return nil, nil, nil
	}
	if mc.capabilities&clientQueryAttributes == 0 {
		if ignoreIfAbsent {
			return nil, nil, nil
		}
		return nil, nil, ErrQueryAttributesNotSupported
	}

	values := make([]driver.Value, len(attrs))
	names := make([]string, len(attrs))
	for i, attr := range attrs {
		value, err := converter{}.ConvertValue(attr.Value)
		if err != nil {
			return nil, nil, err
		}
		values[i] = value
		names[i] = attr.Name
	}
	return values, names, nil
}

type encodedBinaryValues struct {
	nullMask []byte
	types    []byte
	names    []byte
	values   []byte
	longData []stmtLongData
}

func (mc *mysqlConn) encodeBinaryValues(args, attrValues []driver.Value, attrNames []string) (encodedBinaryValues, error) {
	argCount := len(args)
	attrCount := len(attrValues)
	totalCount := argCount + attrCount
	encoded := encodedBinaryValues{}
	if totalCount == 0 {
		return encoded, nil
	}

	encoded.nullMask = make([]byte, (totalCount+7)/8)
	encoded.types = make([]byte, totalCount*2)
	encoded.names = make([]byte, 0, len(attrNames)*8)
	for _, name := range attrNames {
		encoded.names = appendLengthEncodedString(encoded.names, name)
	}

	longDataSize := max(mc.maxAllowedPacket/(argCount+1), 64)
	for i := 0; i < totalCount; i++ {
		var arg driver.Value
		if i < argCount {
			arg = args[i]
		} else {
			arg = attrValues[i-argCount]
		}

		if arg == nil {
			encoded.nullMask[i/8] |= 1 << (uint(i) & 7)
			encoded.types[i*2] = byte(fieldTypeNULL)
			continue
		}

		if v, ok := arg.(json.RawMessage); ok {
			arg = []byte(v)
		}
		if v, ok := arg.([]byte); ok && v == nil {
			encoded.nullMask[i/8] |= 1 << (uint(i) & 7)
			encoded.types[i*2] = byte(fieldTypeNULL)
			continue
		}

		var longData *stmtLongData
		var err error
		encoded.types[i*2], encoded.types[i*2+1], encoded.values, longData, err =
			mc.appendBinaryValue(encoded.values, arg, i, longDataSize, i >= argCount)
		if err != nil {
			return encodedBinaryValues{}, err
		}
		if longData != nil {
			encoded.longData = append(encoded.longData, *longData)
		}
	}
	return encoded, nil
}

func (mc *mysqlConn) appendBinaryValue(dest []byte, arg driver.Value, paramID, longDataSize int, isQueryAttribute bool) (byte, byte, []byte, *stmtLongData, error) {
	switch v := arg.(type) {
	case int64:
		dest = binary.LittleEndian.AppendUint64(dest, uint64(v))
		return byte(fieldTypeLongLong), 0, dest, nil, nil

	case uint64:
		dest = binary.LittleEndian.AppendUint64(dest, v)
		return byte(fieldTypeLongLong), 0x80, dest, nil, nil

	case float64:
		dest = binary.LittleEndian.AppendUint64(dest, math.Float64bits(v))
		return byte(fieldTypeDouble), 0, dest, nil, nil

	case bool:
		if v {
			dest = append(dest, 0x01)
		} else {
			dest = append(dest, 0x00)
		}
		return byte(fieldTypeTiny), 0, dest, nil, nil

	case []byte:
		if isQueryAttribute || len(v) < longDataSize {
			dest = appendLengthEncodedInteger(dest, uint64(len(v)))
			dest = append(dest, v...)
			return byte(fieldTypeString), 0, dest, nil, nil
		}
		return byte(fieldTypeString), 0, dest, &stmtLongData{paramID: paramID, data: v}, nil

	case string:
		if isQueryAttribute || len(v) < longDataSize {
			dest = appendLengthEncodedInteger(dest, uint64(len(v)))
			dest = append(dest, v...)
			return byte(fieldTypeString), 0, dest, nil, nil
		}
		return byte(fieldTypeString), 0, dest, &stmtLongData{paramID: paramID, text: v}, nil

	case time.Time:
		var value [64]byte
		var valueBytes = value[:0]
		if v.IsZero() {
			valueBytes = append(valueBytes, "0000-00-00"...)
		} else {
			var err error
			valueBytes, err = appendDateTime(valueBytes, v.In(mc.cfg.Loc), mc.cfg.timeTruncate)
			if err != nil {
				return 0, 0, nil, nil, err
			}
		}
		dest = appendLengthEncodedInteger(dest, uint64(len(valueBytes)))
		dest = append(dest, valueBytes...)
		return byte(fieldTypeString), 0, dest, nil, nil

	default:
		return 0, 0, nil, nil, fmt.Errorf("cannot convert type: %T", arg)
	}
}

func (mc *mysqlConn) writeQueryPacket(query string, attrValues []driver.Value, attrNames []string) error {
	data := make([]byte, 4, 4+1+len(query)+64)
	data = append(data, comQuery)
	if mc.capabilities&clientQueryAttributes != 0 {
		data = appendLengthEncodedInteger(data, 1)
		data = appendLengthEncodedInteger(data, uint64(len(attrValues)))
		if len(attrValues) > 0 {
			encoded, err := mc.encodeBinaryValues(nil, attrValues, attrNames)
			if err != nil {
				return err
			}
			data = append(data, encoded.nullMask...)
			data = append(data, 0x01)
			data = append(data, encoded.types...)
			data = append(data, encoded.names...)
			data = append(data, encoded.values...)
		}
	}
	data = append(data, query...)

	mc.resetSequence()
	err := mc.writePacket(data)
	mc.syncSequence()
	return err
}
