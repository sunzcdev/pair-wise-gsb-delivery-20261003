// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

// Filter represents a filter in an xz block header.
//
// The only implementations supported by this package are the exported
// filter types [DeltaFilter] and [X86Filter]. The unexported filter
// method keeps the interface sealed for use by [WriterConfig].
type Filter interface {
	filter()
}

// toFilter converts the exported Filter into the internal filter
// representation used by the block header functions.
func toFilter(g Filter) filter {
	switch f := g.(type) {
	case *DeltaFilter:
		return f
	case *X86Filter:
		return f
	}
	return nil
}
