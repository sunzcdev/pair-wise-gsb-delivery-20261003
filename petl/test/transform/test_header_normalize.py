from __future__ import absolute_import, print_function, division

import unicodedata

import pytest

import petl as etl
from petl.errors import HeaderNormalizationError, ArgumentError
from petl.test.helpers import ieq
from petl.transform.headers import (
    normalizeheader, HeaderNormalizer, HeaderMapping,
)


# ---------- individual, composable normalization steps ----------

def test_no_steps_leaves_header_unchanged():
    table = ((' Foo ', 'BAR', 'bar'), ('a', 1, 2))
    result = normalizeheader(table)
    expect = ((' Foo ', 'BAR', 'bar'), ('a', 1, 2))
    ieq(expect, result)
    ieq(expect, result)  # repeatable


def test_strip_edges_only():
    normalizer = HeaderNormalizer(strip=True)
    normalized, mapping = normalizer.normalize(('\tfoo ', ' bar\n', 'a b'))
    assert normalized == ('foo', 'bar', 'a b')
    assert mapping.original_names() == ('\tfoo ', ' bar\n', 'a b')


def test_casefold():
    normalizer = HeaderNormalizer(casefold=True)
    normalized, _ = normalizer.normalize(('Straße', 'FFI', 'ABC'))
    assert normalized == ('strasse', 'ffi', 'abc')


def test_unicode_nfc_nfd_equivalence():
    # e-acute as a single code point vs. e + combining acute accent
    composed = 'é'
    decomposed = 'e\u0301'
    nfc = HeaderNormalizer(unicode_form='NFC')
    nfd = HeaderNormalizer(unicode_form='NFD')
    assert nfc.normalize((decomposed,))[0] == (composed,)
    assert nfd.normalize((composed,))[0] == (unicodedata.normalize('NFD',
                                                                    composed),)
    # combining characters collapse together under NFC + casefold
    both = HeaderNormalizer(unicode_form='NFC', casefold=True)
    with pytest.raises(HeaderNormalizationError):
        both.normalize(('É', 'é'))
    normalized, _ = HeaderNormalizer(
        unicode_form='NFC', casefold=True, disambiguate=True
    ).normalize(('É', 'é'))
    assert normalized == ('é', 'é_1')


def test_nfkc_compatibility_decomposition():
    normalizer = HeaderNormalizer(unicode_form='NFKC')
    normalized, _ = normalizer.normalize(('ﬁ', '①'))
    assert normalized == ('fi', '1')


def test_step_order_strip_then_casefold():
    normalizer = HeaderNormalizer(strip=True, casefold=True)
    with pytest.raises(HeaderNormalizationError):
        normalizer.normalize((' Foo ', 'FOO'))
    normalized, mapping = HeaderNormalizer(
        strip=True, casefold=True, disambiguate=True
    ).normalize((' Foo ', 'FOO'))
    assert normalized == ('foo', 'foo_1')
    # mapping still keeps the originals by position
    assert mapping.original_at(0) == ' Foo '
    assert mapping.original_at(1) == 'FOO'


def test_invalid_unicode_form_rejected():
    with pytest.raises(ArgumentError):
        HeaderNormalizer(unicode_form='BOGUS')


def test_invalid_suffix_rejected():
    with pytest.raises(ArgumentError):
        HeaderNormalizer(disambiguate=True, suffix='')


# ---------- mapping and reversibility ----------

def test_mapping_recovers_original_by_position():
    normalizer = HeaderNormalizer(strip=True, casefold=True,
                                  disambiguate=True)
    header = (' Foo ', 'foo', 'FOO', 'bar')
    normalized, mapping = normalizer.normalize(header)
    assert normalized == ('foo', 'foo_1', 'foo_2', 'bar')
    assert isinstance(mapping, HeaderMapping)
    assert mapping.original_names() == header
    for index, original in enumerate(header):
        assert mapping.original_at(index) == original
        assert mapping.normalized_at(index) == normalized[index]
    assert list(mapping.pairs()) == list(zip(header, normalized))
    assert mapping.normalized_names() == normalized


def test_mapping_asdict_for_unique_originals():
    normalizer = HeaderNormalizer(strip=True)
    _, mapping = normalizer.normalize(('a ', ' b'))
    assert mapping.asdict() == {'a ': 'a', ' b': 'b'}


def test_mapping_asdict_rejects_duplicate_originals():
    normalizer = HeaderNormalizer(disambiguate=True)
    _, mapping = normalizer.normalize(('foo', 'foo'))
    with pytest.raises(ValueError):
        mapping.asdict()
    # position-based access still works despite duplicate originals
    assert mapping.original_at(0) == 'foo'
    assert mapping.original_at(1) == 'foo'


def test_mapping_is_tuple_of_triples():
    normalizer = HeaderNormalizer(strip=True)
    _, mapping = normalizer.normalize(('a ',))
    assert tuple(mapping) == ((0, 'a ', 'a'),)


# ---------- collisions ----------

def test_collision_rejected_before_data_rows():
    table = iter([('foo', 'FOO'), ('data', 1)])
    result = normalizeheader(table, casefold=True)
    iterator = iter(result)
    with pytest.raises(HeaderNormalizationError) as excinfo:
        next(iterator)
    message = str(excinfo.value)
    assert 'foo' in message
    # the data row must never have been consumed
    assert list(iterator) == []


def test_collision_error_lists_all_positions():
    normalizer = HeaderNormalizer(casefold=True)
    with pytest.raises(HeaderNormalizationError) as excinfo:
        normalizer.normalize(('A', 'a', 'b', 'B'))
    assert excinfo.value.collisions == {'a': [0, 1], 'b': [2, 3]}


def test_disambiguate_stable_in_column_order():
    normalizer = HeaderNormalizer(casefold=True, disambiguate=True)
    normalized, mapping = normalizer.normalize(('A', 'a', 'a', 'B'))
    assert normalized == ('a', 'a_1', 'a_2', 'b')
    assert mapping.original_at(2) == 'a'


def test_disambiguate_does_not_recollide_with_existing_suffix():
    normalizer = HeaderNormalizer(casefold=True, disambiguate=True)
    normalized, _ = normalizer.normalize(('foo', 'FOO', 'foo_1'))
    # 'foo_1' already exists, so the duplicate must take 'foo_2'
    assert normalized == ('foo', 'foo_2', 'foo_1')


def test_disambiguate_skips_chained_existing_suffixes():
    normalizer = HeaderNormalizer(disambiguate=True)
    normalized, _ = normalizer.normalize(
        ('x', 'x', 'x_1', 'x_2', 'x_3')
    )
    assert normalized == ('x', 'x_4', 'x_1', 'x_2', 'x_3')
    assert len(set(normalized)) == len(normalized)


def test_disambiguate_repeated_empty_names():
    normalizer = HeaderNormalizer(strip=True, disambiguate=True)
    normalized, mapping = normalizer.normalize(('', ' ', '\t', ''))
    assert normalized == ('', '_1', '_2', '_3')
    assert mapping.original_names() == ('', ' ', '\t', '')


def test_disambiguate_empty_name_with_existing_suffix():
    normalizer = HeaderNormalizer(disambiguate=True)
    normalized, _ = normalizer.normalize(('', '_1', ''))
    # the pre-existing '_1' is reserved, so the later empty name skips it
    assert normalized == ('', '_1', '_2')
    assert len(set(normalized)) == len(normalized)


def test_custom_suffix():
    normalizer = HeaderNormalizer(casefold=True, disambiguate=True,
                                  suffix='.')
    normalized, _ = normalizer.normalize(('a', 'A'))
    assert normalized == ('a', 'a.1')


def test_no_collision_when_names_stay_distinct():
    normalizer = HeaderNormalizer(strip=True)
    normalized, mapping = normalizer.normalize(('foo', 'bar'))
    assert normalized == ('foo', 'bar')
    assert len(mapping) == 2


# ---------- non-string headers ----------

def test_non_string_header_raises_type_error():
    normalizer = HeaderNormalizer(strip=True)
    with pytest.raises(TypeError) as excinfo:
        normalizer.normalize(('foo', 42))
    assert 'column 1' in str(excinfo.value)


def test_none_header_raises_type_error():
    normalizer = HeaderNormalizer()
    with pytest.raises(TypeError):
        normalizer.normalize((None, 'foo'))


def test_bytes_header_not_coerced():
    normalizer = HeaderNormalizer()
    with pytest.raises(TypeError):
        normalizer.normalize((b'foo',))


def test_non_string_header_error_in_view():
    table = (('foo', 1), ('a', 'b'))
    with pytest.raises(TypeError):
        list(normalizeheader(table, strip=True))


# ---------- table view behaviour ----------

def test_normalizeheader_data_rows_unchanged():
    table = ((' Foo ', 'BAR'), ('a', 1), ('b', 2), ('c', None))
    result = normalizeheader(table, strip=True, casefold=True,
                             disambiguate=True)
    rows = list(result)
    assert rows[0] == ('foo', 'bar')
    assert rows[1:] == [('a', 1), ('b', 2), ('c', None)]


def test_normalizeheader_fluent_method():
    table = etl.wrap(((' A ',), ('x',)))
    result = table.normalizeheader(strip=True, casefold=True)
    ieq((('a',), ('x',)), result)


def test_normalizeheader_empty_table():
    table = iter(())
    assert list(normalizeheader(table, strip=True)) == []


def test_normalizeheader_reusable_normalizer():
    from petl.transform.headers import NormalizeHeaderView
    normalizer = HeaderNormalizer(strip=True, casefold=True,
                                  disambiguate=True)
    table1 = ((' Foo ', 'foo'), (1, 2))
    table2 = (('BAR', 'bar'), (3, 4))
    ieq((('foo', 'foo_1'), (1, 2)),
        NormalizeHeaderView(table1, normalizer))
    ieq((('bar', 'bar_1'), (3, 4)),
        NormalizeHeaderView(table2, normalizer))


def test_normalizeheader_unicode_combining_end_to_end():
    # decomposed + composed equivalents in an actual table header
    table = (('e\u0301', 'É'), (1, 2))
    result = normalizeheader(table, unicode_form='NFC', casefold=True,
                             disambiguate=True)
    rows = list(result)
    assert rows[0] == ('é', 'é_1')
    assert rows[1] == (1, 2)