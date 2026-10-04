// Copyright 2026 Unknwon
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package ini

import (
	"fmt"
	"io"
	"sort"
)

// DiffKey describes the desired raw state of a single key. Values holds the
// raw value followed by shadow values in their original order. An empty
// Values[0] means the key is explicitly set to an empty value, which is not
// the same as deleting the key.
type DiffKey struct {
	Name          string
	Values        []string
	Nested        []string
	Boolean       bool
	AutoIncrement bool
}

// DiffSet describes additions and value changes for one section instance.
// Index is the occurrence index of the section name (relevant when
// AllowNonUniqueSections is used). NewSection is true when the whole section
// instance does not exist in the base file.
type DiffSet struct {
	Section    string
	Index      int
	NewSection bool
	Keys       []DiffKey

	// IsRaw and RawBody cover sections loaded via UnparseableSections.
	IsRaw   bool
	RawBody string
}

// DiffDeletion describes content that exists in the base file but not in the
// modified file. An empty Key means the whole section instance (identified by
// Section and Index) was removed.
type DiffDeletion struct {
	Section string
	Index   int
	Key     string
}

// String returns a human-readable representation of the deletion.
func (d DiffDeletion) String() string {
	if d.Key == "" {
		return fmt.Sprintf("[%s#%d] (entire section)", d.Section, d.Index)
	}
	return fmt.Sprintf("[%s#%d] %s", d.Section, d.Index, d.Key)
}

// Diff is the difference between a base File and a modified File. Sets
// contains additions and changed values in a stable order and can be rendered
// as a regular INI document via Overlay or WriteTo. Deletions is a separate
// list of base content that no longer exists; an empty value is never used to
// express a deletion. Comments are not compared nor exported.
//
// Applying a Diff to the base file with ApplyTo restores the effective key
// set and duplicate section instance order of the modified file.
type Diff struct {
	Sets      []DiffSet
	Deletions []DiffDeletion
}

// DiffFiles compares the base file with the modified file and returns the
// minimal difference. Comparison is done on raw values (Key.Value), shadow
// value order, and duplicate section instances. ValueMapper and value
// interpolation are never consulted.
func DiffFiles(base, modified *File) *Diff {
	d := &Diff{}

	// Additions and changes, in the modified file's section order.
	modCounts := make(map[string]int)
	for _, msec := range modified.Sections() {
		name := msec.name
		idx := modCounts[name]
		modCounts[name]++

		bsecs := base.sections[name]
		if idx >= len(bsecs) {
			set := DiffSet{Section: name, Index: idx, NewSection: true}
			if msec.isRawSection {
				set.IsRaw = true
				set.RawBody = msec.rawBody
			} else {
				for _, kname := range msec.keyList {
					set.Keys = append(set.Keys, diffKeyFrom(msec.keys[kname]))
				}
			}
			d.Sets = append(d.Sets, set)
			continue
		}

		bsec := bsecs[idx]
		if bsec.isRawSection || msec.isRawSection {
			if bsec.isRawSection != msec.isRawSection || bsec.rawBody != msec.rawBody {
				set := DiffSet{Section: name, Index: idx}
				if msec.isRawSection {
					set.IsRaw = true
					set.RawBody = msec.rawBody
				}
				d.Sets = append(d.Sets, set)
			}
			continue
		}

		var set *DiffSet
		for _, kname := range msec.keyList {
			mkey := msec.keys[kname]
			bkey := bsec.keys[kname]
			if bkey == nil || !keysRawEqual(bkey, mkey) {
				if set == nil {
					set = &DiffSet{Section: name, Index: idx}
				}
				set.Keys = append(set.Keys, diffKeyFrom(mkey))
			}
		}
		if set != nil {
			d.Sets = append(d.Sets, *set)
		}

		// Keys present in the base instance but gone from the modified one.
		for _, kname := range bsec.keyList {
			if msec.keys[kname] == nil {
				d.Deletions = append(d.Deletions, DiffDeletion{Section: name, Index: idx, Key: kname})
			}
		}
	}

	// Section instances present in the base file but gone from the modified
	// file, in the base file's section order.
	baseCounts := make(map[string]int)
	for _, bsec := range base.Sections() {
		name := bsec.name
		idx := baseCounts[name]
		baseCounts[name]++
		if idx >= modCounts[name] {
			d.Deletions = append(d.Deletions, DiffDeletion{Section: name, Index: idx})
		}
	}

	return d
}

// DiffFrom is a shorthand for DiffFiles(base, f).
func (f *File) DiffFrom(base *File) *Diff {
	return DiffFiles(base, f)
}

// IsEmpty reports whether the diff contains no changes at all.
func (d *Diff) IsEmpty() bool {
	return len(d.Sets) == 0 && len(d.Deletions) == 0
}

// Overlay returns the additions and changed values as a regular INI File
// that can be written with WriteTo or SaveTo. Deletions are not part of the
// overlay; use the Deletions list instead.
func (d *Diff) Overlay() *File {
	overlay := Empty()
	overlay.options.AllowNonUniqueSections = true

	counts := make(map[string]int)
	for _, set := range d.Sets {
		var sec *Section
		if counts[set.Section] == 0 {
			// Reuse the default section created by Empty when applicable.
			sec = overlay.SectionWithIndex(set.Section, 0)
		} else {
			sec, _ = overlay.NewSection(set.Section)
		}
		counts[set.Section]++

		if set.IsRaw {
			sec.isRawSection = true
			sec.rawBody = set.RawBody
			continue
		}
		for _, dkey := range set.Keys {
			setKeyState(sec, dkey)
		}
	}
	return overlay
}

// WriteTo writes the overlay (additions and changed values) as a regular INI
// document into w.
func (d *Diff) WriteTo(w io.Writer) (int64, error) {
	return d.Overlay().WriteTo(w)
}

// ApplyTo applies the diff to the given base file, restoring the effective
// key set and duplicate section instance order of the modified file the diff
// was computed from.
func (d *Diff) ApplyTo(base *File) error {
	// Allow creating and deleting duplicate section instances regardless of
	// how the base file was loaded.
	oldNonUnique := base.options.AllowNonUniqueSections
	base.options.AllowNonUniqueSections = true
	defer func() { base.options.AllowNonUniqueSections = oldNonUnique }()

	// Key deletions reference section instances by their original index, so
	// they must be applied before any section instance is removed.
	for _, del := range d.Deletions {
		if del.Key == "" {
			continue
		}
		secs, err := base.SectionsByName(del.Section)
		if err != nil || del.Index >= len(secs) {
			continue
		}
		secs[del.Index].DeleteKey(del.Key)
	}

	// Delete section instances per name from the highest index down, so
	// earlier removals do not shift the indexes of later ones.
	sectionDeletions := make(map[string][]int)
	var names []string
	for _, del := range d.Deletions {
		if del.Key != "" {
			continue
		}
		if _, ok := sectionDeletions[del.Section]; !ok {
			names = append(names, del.Section)
		}
		sectionDeletions[del.Section] = append(sectionDeletions[del.Section], del.Index)
	}
	for _, name := range names {
		indexes := sectionDeletions[name]
		sort.Sort(sort.Reverse(sort.IntSlice(indexes)))
		for _, idx := range indexes {
			_ = base.DeleteSectionWithIndex(name, idx)
		}
	}

	// Additions and changes.
	for _, set := range d.Sets {
		sec, err := sectionInstanceForWrite(base, set.Section, set.Index)
		if err != nil {
			return err
		}

		if base.BlockMode {
			base.lock.Lock()
		}
		if set.IsRaw {
			sec.isRawSection = true
			sec.rawBody = set.RawBody
		} else {
			for _, dkey := range set.Keys {
				setKeyState(sec, dkey)
			}
		}
		if base.BlockMode {
			base.lock.Unlock()
		}
	}
	return nil
}

// diffKeyFrom captures the raw state of a key.
func diffKeyFrom(k *Key) DiffKey {
	dkey := DiffKey{
		Name:          k.name,
		Values:        []string{k.value},
		Boolean:       k.isBooleanType,
		AutoIncrement: k.isAutoIncrement,
	}
	for _, shadow := range k.shadows {
		dkey.Values = append(dkey.Values, shadow.value)
	}
	dkey.Nested = append(dkey.Nested, k.nestedValues...)
	return dkey
}

// keysRawEqual reports whether two keys hold the same raw state: raw value,
// shadow values in order, nested values, and type flags.
func keysRawEqual(a, b *Key) bool {
	if a.value != b.value ||
		a.isBooleanType != b.isBooleanType ||
		a.isAutoIncrement != b.isAutoIncrement ||
		len(a.shadows) != len(b.shadows) ||
		len(a.nestedValues) != len(b.nestedValues) {
		return false
	}
	for i := range a.shadows {
		if a.shadows[i].value != b.shadows[i].value {
			return false
		}
	}
	for i := range a.nestedValues {
		if a.nestedValues[i] != b.nestedValues[i] {
			return false
		}
	}
	return true
}

// setKeyState creates or overwrites a key in the section so that its raw
// state exactly matches the given DiffKey. The caller is responsible for
// locking.
func setKeyState(sec *Section, dkey DiffKey) {
	key := sec.keys[dkey.Name]
	if key == nil {
		key = newKey(sec, dkey.Name, "")
		sec.keyList = append(sec.keyList, dkey.Name)
		sec.keys[dkey.Name] = key
	}

	val := ""
	if len(dkey.Values) > 0 {
		val = dkey.Values[0]
	}
	key.value = val
	sec.keysHash[dkey.Name] = val
	key.isBooleanType = dkey.Boolean
	key.isAutoIncrement = dkey.AutoIncrement

	key.shadows = nil
	for _, shadowVal := range dkey.Values[1:] {
		shadow := newKey(sec, dkey.Name, shadowVal)
		shadow.isShadow = true
		key.shadows = append(key.shadows, shadow)
	}
	key.nestedValues = append([]string(nil), dkey.Nested...)
}

// sectionInstanceForWrite returns the section instance with the given name
// and occurrence index, creating instances as needed.
func sectionInstanceForWrite(f *File, name string, index int) (*Section, error) {
	for {
		secs, err := f.SectionsByName(name)
		if err == nil && len(secs) > index {
			return secs[index], nil
		}
		if _, err := f.NewSection(name); err != nil {
			return nil, err
		}
	}
}
