// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2021 Datadog, Inc.

package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFoldedBinStoreCollapsingLowest(t *testing.T) {
	s := NewCollapsingLowestDenseStore(4)
	for _, index := range []int{100, 101, 102, 103, 104} {
		s.Add(index)
	}
	f, ok := Store(s).(FoldedBinStore)
	assert.True(t, ok)
	minIndex, err := s.MinIndex()
	assert.NoError(t, err)
	maxIndex, err := s.MaxIndex()
	assert.NoError(t, err)

	below, above := f.IsBinFolded(minIndex)
	assert.True(t, below)
	assert.False(t, above)
	for index := minIndex + 1; index <= maxIndex; index++ {
		below, above = f.IsBinFolded(index)
		assert.False(t, below)
		assert.False(t, above)
	}
}

func TestFoldedBinStoreCollapsingHighest(t *testing.T) {
	s := NewCollapsingHighestDenseStore(4)
	for _, index := range []int{100, 101, 102, 103, 104} {
		s.Add(index)
	}
	f, ok := Store(s).(FoldedBinStore)
	assert.True(t, ok)
	minIndex, err := s.MinIndex()
	assert.NoError(t, err)
	maxIndex, err := s.MaxIndex()
	assert.NoError(t, err)

	below, above := f.IsBinFolded(maxIndex)
	assert.False(t, below)
	assert.True(t, above)
	for index := minIndex; index < maxIndex; index++ {
		below, above = f.IsBinFolded(index)
		assert.False(t, below)
		assert.False(t, above)
	}
}

func TestFoldedBinStoreFullyCollapsed(t *testing.T) {
	// A store wider than the number of bins collapses everything into one bin.
	for _, constructor := range []func(int) Store{
		func(maxNumBins int) Store { return NewCollapsingLowestDenseStore(maxNumBins) },
		func(maxNumBins int) Store { return NewCollapsingHighestDenseStore(maxNumBins) },
	} {
		s := constructor(1)
		s.AddWithCount(0, 1)
		s.AddWithCount(1000, 1)
		f := s.(FoldedBinStore)

		extreme, err := s.MinIndex()
		assert.NoError(t, err)
		below, above := f.IsBinFolded(extreme)
		assert.True(t, below)
		assert.True(t, above)
	}
}

func TestFoldedBinStoreNonCollapsingStores(t *testing.T) {
	for _, s := range []Store{
		NewDenseStore(),
		NewBufferedPaginatedStore(),
		NewSparseStore(),
	} {
		s.Add(0)
		_, ok := s.(FoldedBinStore)
		assert.False(t, ok)
	}
}
