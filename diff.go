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
	"strings"
)

// DiffOptions customizes the behavior of Diff.
type DiffOptions struct {
	// WithComments indicates whether comments of added or changed sections
	// and keys are carried over to the overlay. Comment-only changes are
	// never treated as differences.
	WithComments bool
}

// DeletionEntry identifies a piece of content that exists in the base file
// but is absent from the modified file. Deletions are reported separately
// from the overlay because an empty value is a valid value and must not be
// confused with a removal.
type DeletionEntry struct {
	// Section is the name of the section the deletion applies to.
	Section string
	// Index is the instance index among sections sharing the same name
	// (always 0 unless non-unique sections are used).
	Index int
	// Key is the name of the deleted key. An empty Key means the whole
	// section instance identified by Section and Index was removed.
	Key string
}

// String returns a human-readable representation of the deletion entry.
func (d DeletionEntry) String() string {
	if d.Key == "" {
		return fmt.Sprintf("section %q instance %d", d.Section, d.Index)
	}
	return fmt.Sprintf("key %q in section %q instance %d", d.Key, d.Section, d.Index)
}

// FileDiff describes the differences between a base File and a modified File.
//
// Overlay is an ordinary *File containing only added and changed sections and
// keys, and can be written out as regular INI content via WriteTo or SaveTo.
// Deletions lists content of the base file that no longer exists in the
// modified file.
//
// Applying a FileDiff to the base file with ApplyDiff restores the effective
// key set of the modified file, including shadow order and the relative order
// of duplicate section instances.
type FileDiff struct {
	Overlay   *File
	Deletions []DeletionEntry

	// targets is aligned with Overlay.sectionList and records the base file
	// section instance index each overlay section applies to, or -1 when the
	// overlay section is a new instance to be appended.
	targets []int
}

// Empty reports whether the diff contains no differences.
func (d *FileDiff) Empty() bool {
	return len(d.Deletions) == 0 && len(d.Overlay.sectionList) == 0
}

// Diff compares the base file against the modified file and returns the
// minimal set of changes that transforms the base file into the modified
// file.
//
// The comparison is performed on raw key values (Key.Value), shadow values in
// their original order, nested values, and duplicate section instances. It
// never invokes ValueMapper or performs value interpolation, so differences
// are derived from stored content rather than guessed from expanded results.
//
// Both files are only read. The export order is stable: overlay sections and
// keys follow the order of the modified file, and deletions follow the order
// of the base file.
func Diff(base, modified *File, opts ...DiffOptions) *FileDiff {
	var opt DiffOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	overlayOpts := modified.options
	// Force options required to faithfully represent the modified content.
	overlayOpts.AllowShadows = true
	overlayOpts.AllowNonUniqueSections = true
	overlayOpts.AllowDuplicateShadowValues = true
	overlay := newFile(nil, overlayOpts)

	d := &FileDiff{Overlay: overlay}

	baseByName := sectionsByName(base)
	modByName := sectionsByName(modified)

	// Pending overlay sections lazily created per (name, instance index).
	type overlayKey struct {
		name  string
		index int
	}
	pending := make(map[overlayKey]*Section)
	overlaySection := func(name string, index int, create bool) *Section {
		key := overlayKey{name, index}
		if sec, ok := pending[key]; ok {
			return sec
		}
		if !create {
			return nil
		}
		// The target index is the base instance index, or -1 for instances
		// that only exist in the modified file.
		target := index
		if index >= len(baseByName[name]) {
			target = -1
		}
		sec, _ := overlay.NewSection(name)
		d.targets = append(d.targets, target)
		pending[key] = sec
		return sec
	}

	// Walk the modified file in order to collect additions and changes.
	modCount := make(map[string]int)
	for _, ms := range modified.Sections() {
		index := modCount[ms.name]
		modCount[ms.name]++

		baseInstances := baseByName[ms.name]
		if index >= len(baseInstances) {
			// The whole section instance is new.
			copySection(overlaySection(ms.name, index, true), ms, opt)
			continue
		}

		bs := baseInstances[index]
		if ms.isRawSection || bs.isRawSection {
			if !bs.isRawSection || !ms.isRawSection || bs.rawBody != ms.rawBody {
				osec := overlaySection(ms.name, index, true)
				osec.isRawSection = true
				osec.rawBody = ms.rawBody
				if opt.WithComments {
					osec.Comment = ms.Comment
				}
			}
			continue
		}

		for _, kname := range ms.keyList {
			mk := ms.keys[kname]
			bk := bs.keys[kname]
			if bk != nil && keysEqual(bk, mk) {
				continue
			}
			copyKey(overlaySection(ms.name, index, true), mk, opt)
		}
		if opt.WithComments {
			if osec := overlaySection(ms.name, index, false); osec != nil {
				osec.Comment = ms.Comment
			}
		}
	}

	// Walk the base file in order to collect deletions.
	baseCount := make(map[string]int)
	for _, bs := range base.Sections() {
		index := baseCount[bs.name]
		baseCount[bs.name]++

		modInstances := modByName[bs.name]
		if index >= len(modInstances) {
			d.Deletions = append(d.Deletions, DeletionEntry{Section: bs.name, Index: index})
			continue
		}

		ms := modInstances[index]
		if bs.isRawSection || ms.isRawSection {
			continue // Handled as a change via the overlay.
		}
		for _, kname := range bs.keyList {
			if _, ok := ms.keys[kname]; !ok {
				d.Deletions = append(d.Deletions, DeletionEntry{Section: bs.name, Index: index, Key: kname})
			}
		}
	}

	return d
}

// ApplyDiff applies the diff to the file, transforming its content into the
// modified file the diff was computed from. Deletions are applied first,
// followed by the overlay. The file should have been loaded with options
// compatible with the ones used for the files passed to Diff (e.g.
// AllowShadows, AllowNonUniqueSections).
func (f *File) ApplyDiff(d *FileDiff) error {
	// Apply deletions in reverse order so that instance indices recorded
	// against the base file remain valid while earlier deletions take effect.
	for i := len(d.Deletions) - 1; i >= 0; i-- {
		del := d.Deletions[i]
		if del.Key == "" {
			if err := f.DeleteSectionWithIndex(del.Section, del.Index); err != nil {
				return fmt.Errorf("apply deletion %q: %w", del.String(), err)
			}
			continue
		}
		secs, err := f.SectionsByName(del.Section)
		if err != nil || len(secs) <= del.Index {
			continue // Already gone.
		}
		secs[del.Index].DeleteKey(del.Key)
	}

	// Deleted section instances shift the indices of subsequent instances
	// with the same name, so adjust overlay targets accordingly.
	targets := make([]int, len(d.targets))
	copy(targets, d.targets)
	for _, del := range d.Deletions {
		if del.Key != "" {
			continue
		}
		for i, sname := range d.Overlay.sectionList {
			if sname == del.Section && targets[i] > del.Index {
				targets[i]--
			}
		}
	}

	for i, sname := range d.Overlay.sectionList {
		osec := d.Overlay.sections[sname][d.Overlay.sectionIndexes[i]]

		var sec *Section
		if i < len(targets) && targets[i] >= 0 {
			if secs, err := f.SectionsByName(sname); err == nil && targets[i] < len(secs) {
				sec = secs[targets[i]]
			}
		}
		if sec == nil {
			var err error
			sec, err = f.NewSection(sname)
			if err != nil {
				return fmt.Errorf("apply overlay section %q: %w", sname, err)
			}
		}

		if osec.isRawSection {
			sec.isRawSection = true
			sec.rawBody = osec.rawBody
			continue
		}

		for _, kname := range osec.keyList {
			okey := osec.keys[kname]
			sec.DeleteKey(kname)
			nk, err := sec.NewKey(kname, okey.value)
			if err != nil {
				return fmt.Errorf("apply overlay key %q in section %q: %w", kname, sname, err)
			}
			for _, shadow := range okey.shadows {
				// Append directly to bypass deduplication and option checks,
				// so shadow values and their order are preserved exactly.
				sk := newKey(sec, kname, shadow.value)
				sk.isShadow = true
				nk.shadows = append(nk.shadows, sk)
			}
			nk.isBooleanType = okey.isBooleanType
			nk.nestedValues = append([]string(nil), okey.nestedValues...)
		}
	}
	return nil
}

// sectionsByName groups the sections of the file by name, preserving the
// instance order of duplicate sections.
func sectionsByName(f *File) map[string][]*Section {
	grouped := make(map[string][]*Section)
	for _, sec := range f.Sections() {
		grouped[sec.name] = append(grouped[sec.name], sec)
	}
	return grouped
}

// keysEqual reports whether two keys store identical raw content: value,
// shadow values in order, nested values, and key type flags. It deliberately
// does not use Key.String, so neither ValueMapper nor value interpolation
// affects the comparison.
func keysEqual(a, b *Key) bool {
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

// copySection copies the full content of a section into the overlay section.
func copySection(dst, src *Section, opt DiffOptions) {
	if src.isRawSection {
		dst.isRawSection = true
		dst.rawBody = src.rawBody
	} else {
		for _, kname := range src.keyList {
			copyKey(dst, src.keys[kname], opt)
		}
	}
	if opt.WithComments {
		dst.Comment = src.Comment
	}
}

// copyKey copies the raw content of a key into the overlay section.
func copyKey(dst *Section, src *Key, opt DiffOptions) {
	nk, _ := dst.NewKey(src.name, src.value)
	for _, shadow := range src.shadows {
		sk := newKey(dst, src.name, shadow.value)
		sk.isShadow = true
		nk.shadows = append(nk.shadows, sk)
	}
	nk.isBooleanType = src.isBooleanType
	nk.isAutoIncrement = src.isAutoIncrement
	nk.nestedValues = append([]string(nil), src.nestedValues...)
	if opt.WithComments {
		nk.Comment = src.Comment
	}
}

// String returns a human-readable summary of the diff.
func (d *FileDiff) String() string {
	var buf strings.Builder
	buf.WriteString("overlay:\n")
	if overlay, err := d.Overlay.writeToBuffer(""); err == nil {
		buf.Write(overlay.Bytes())
	}
	for _, del := range d.Deletions {
		buf.WriteString("delete: " + del.String() + "\n")
	}
	return buf.String()
}
