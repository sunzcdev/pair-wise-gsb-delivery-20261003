package text

import "slices"

// CloneSingleLineValue returns a copy of v that does not share v's position
// slice. Owned strings and decoders are immutable and are shared safely.
func CloneSingleLineValue(v SingleLineValue) SingleLineValue {
	if v.IsOwned() {
		return v
	}
	return NewSingleLineValue(v.Index(), v.decoderOrDefault())
}

// CloneMultiLineValue returns a copy of v that does not share v's position
// slice. Owned strings and decoders are immutable and are shared safely.
func CloneMultiLineValue(v MultiLineValue) MultiLineValue {
	if v.IsOwned() {
		return v
	}
	return NewMultiLineValueFromIndices(slices.Clone(v.Indices()), v.decoderOrDefault())
}

// CloneLines returns a copy of l that does not share l's segment slice.
// Owned strings are immutable and are shared safely.
func CloneLines(l Lines) Lines {
	if l.IsOwned() {
		return l
	}
	return NewLinesFromSegments(slices.Clone(l.Segments()))
}
