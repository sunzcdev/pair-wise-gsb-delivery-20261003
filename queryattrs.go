// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2026 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// QueryAttribute is a MySQL query attribute sent to the server alongside a
// single statement, e.g. a trace ID or the name of the business operation
// that issued the statement. Attributes are transmitted in the COM_QUERY and
// COM_STMT_EXECUTE packets themselves (CLIENT_QUERY_ATTRIBUTES capability,
// MySQL 8.0.23+); they are not SQL comments and not statement parameters.
//
// The server exposes them via mysql_query_attribute_string() and the
// performance_schema, where database auditing can pick them up.
//
// Name may be empty and does not need to be unique; attributes are sent in
// the given order. Value must be nil (NULL) or a type accepted by
// [database/sql/driver.DefaultParameterConverter] plus uint64 and
// [encoding/json.RawMessage], mirroring the driver's statement parameters.
type QueryAttribute struct {
	Name  string
	Value any
}

// queryAttributesContextKey is the context key set by WithQueryAttributes.
type queryAttributesContextKey struct{}

// WithQueryAttributes returns a context that carries the given query
// attributes. Passing the returned context to ExecContext or QueryContext of
// database/sql (or of the driver directly) attaches the attributes to that
// single statement only; they are never stored on the connection and never
// leak into subsequent statements.
//
// Attributes are distinct from statement arguments: they are not matched to
// ? placeholders and are not visible to driver.NamedValue handling.
//
// If the server does not support query attributes, statements carrying them
// fail with [ErrQueryAttributesUnsupported], unless the DSN parameter
// queryAttributes=ignore is set, in which case the attributes are dropped.
func WithQueryAttributes(ctx context.Context, attrs ...QueryAttribute) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	// Copy so later mutation of the caller's slice cannot affect the request.
	cp := make([]QueryAttribute, len(attrs))
	copy(cp, attrs)
	return context.WithValue(ctx, queryAttributesContextKey{}, cp)
}

// queryAttributesFromContext returns the attributes set by WithQueryAttributes.
func queryAttributesFromContext(ctx context.Context) []QueryAttribute {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(queryAttributesContextKey{}).([]QueryAttribute)
	return attrs
}

// queryAttribute is a QueryAttribute with its value converted to a
// wire-ready driver.Value.
type queryAttribute struct {
	name  string
	value driver.Value
}

// resolveQueryAttributes extracts the query attributes carried by ctx and
// converts their values for wire encoding. It enforces the explicit policy
// for servers without the CLIENT_QUERY_ATTRIBUTES capability: fail with
// ErrQueryAttributesUnsupported, or drop the attributes when the
// queryAttributes=ignore DSN parameter is set.
func (mc *mysqlConn) resolveQueryAttributes(ctx context.Context) ([]queryAttribute, error) {
	attrs := queryAttributesFromContext(ctx)
	if len(attrs) == 0 {
		return nil, nil
	}
	if mc.capabilities&clientQueryAttributes == 0 {
		if mc.cfg.queryAttributesIgnore {
			return nil, nil
		}
		return nil, ErrQueryAttributesUnsupported
	}
	resolved := make([]queryAttribute, len(attrs))
	for i, attr := range attrs {
		value, err := converter{}.ConvertValue(attr.Value)
		if err != nil {
			return nil, fmt.Errorf("mysql: cannot convert query attribute %q: %w", attr.Name, err)
		}
		resolved[i] = queryAttribute{name: attr.Name, value: value}
	}
	return resolved, nil
}

// lengthEncodedIntegerSize returns the encoded size of n as a
// length-encoded integer.
func lengthEncodedIntegerSize(n uint64) int {
	switch {
	case n <= 250:
		return 1
	case n <= 0xffff:
		return 3
	case n <= 0xffffff:
		return 4
	}
	return 9
}

// encodeQueryAttributes encodes the attribute section of a COM_QUERY packet:
// NULL bitmap, new-params-bound flag, parameter types, parameter names and
// parameter values, in the binary protocol value format.
//
// https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query.html
func encodeQueryAttributes(attrs []queryAttribute, loc *time.Location, timeTruncate time.Duration) ([]byte, error) {
	nullMaskLen := (len(attrs) + 7) / 8
	// NULL bitmap followed by the new-params-bound flag.
	data := make([]byte, nullMaskLen+1)
	nullMask := data[:nullMaskLen]
	data[nullMaskLen] = 0x01 // new-params-bound flag

	types := make([]byte, 0, 2*len(attrs))
	names := make([]byte, 0, 8*len(attrs))
	values := make([]byte, 0, 16*len(attrs))

	for i, attr := range attrs {
		names = appendLengthEncodedString(names, attr.name)

		value := attr.value
		if v, ok := value.(json.RawMessage); ok {
			value = []byte(v)
		}
		switch v := value.(type) {
		case nil:
			nullMask[i/8] |= 1 << (uint(i) & 7)
			types = append(types, byte(fieldTypeNULL), 0x00)

		case int64:
			types = append(types, byte(fieldTypeLongLong), 0x00)
			values = binary.LittleEndian.AppendUint64(values, uint64(v))

		case uint64:
			types = append(types, byte(fieldTypeLongLong), 0x80) // unsigned
			values = binary.LittleEndian.AppendUint64(values, v)

		case float64:
			types = append(types, byte(fieldTypeDouble), 0x00)
			values = binary.LittleEndian.AppendUint64(values, math.Float64bits(v))

		case bool:
			types = append(types, byte(fieldTypeTiny), 0x00)
			if v {
				values = append(values, 0x01)
			} else {
				values = append(values, 0x00)
			}

		case []byte:
			types = append(types, byte(fieldTypeString), 0x00)
			values = appendLengthEncodedInteger(values, uint64(len(v)))
			values = append(values, v...)

		case string:
			types = append(types, byte(fieldTypeString), 0x00)
			values = appendLengthEncodedString(values, v)

		case time.Time:
			types = append(types, byte(fieldTypeString), 0x00)
			var buf []byte
			var err error
			if v.IsZero() {
				buf = []byte("0000-00-00")
			} else {
				buf, err = appendDateTime(nil, v.In(loc), timeTruncate)
				if err != nil {
					return nil, fmt.Errorf("mysql: cannot convert query attribute %q: %w", attr.name, err)
				}
			}
			values = appendLengthEncodedInteger(values, uint64(len(buf)))
			values = append(values, buf...)

		default:
			return nil, fmt.Errorf("mysql: cannot convert query attribute %q of type %T", attr.name, attr.value)
		}
	}

	data = append(data, types...)
	data = append(data, names...)
	return append(data, values...), nil
}
