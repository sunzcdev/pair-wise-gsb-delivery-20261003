# Copyright (c) 2013, Mahmoud Hashemi
#
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions are
# met:
#
#    * Redistributions of source code must retain the above copyright
#      notice, this list of conditions and the following disclaimer.
#
#    * Redistributions in binary form must reproduce the above
#      copyright notice, this list of conditions and the following
#      disclaimer in the documentation and/or other materials provided
#      with the distribution.
#
#    * The names of the contributors may not be used to endorse or
#      promote products derived from this software without specific
#      prior written permission.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
# "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
# LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
# A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
# OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
# SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
# LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
# DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
# THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
# (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
# OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

"""If there is one recurring theme in ``boltons``, it is that Python
has excellent datastructures that constitute a good foundation for
most quick manipulations, as well as building applications. However,
Python usage has grown much faster than builtin data structure
power. Python has a growing need for more advanced general-purpose
data structures which behave intuitively.

The :class:`Table` class is one example. When handed one- or
two-dimensional data, it can provide useful, if basic, text and HTML
renditions of small to medium sized data. It also heuristically
handles recursive data of various formats (lists, dicts, namedtuples,
objects).

For more advanced :class:`Table`-style manipulation check out the
`pandas`_ DataFrame.

.. _pandas: http://pandas.pydata.org/

"""


from html import escape as html_escape
import types
from itertools import islice
from collections.abc import Sequence, Mapping, MutableSequence

try:
    from .typeutils import make_sentinel
    _MISSING = make_sentinel(var_name='_MISSING')
except ImportError:
    _MISSING = object()

"""
Some idle feature thoughts:

* shift around column order without rearranging data
* gotta make it so you can add additional items, not just initialize with
* maybe a shortcut would be to allow adding of Tables to other Tables
* what's the perf of preallocating lists and overwriting items versus
  starting from empty?
* is it possible to effectively tell the difference between when a
  Table is from_data()'d with a single row (list) or with a list of lists?
* CSS: white-space pre-line or pre-wrap maybe?
* Would be nice to support different backends (currently uses lists
  exclusively). Sometimes large datasets come in list-of-dicts and
  list-of-tuples format and it's desirable to cut down processing overhead.

TODO: make iterable on rows?
"""

__all__ = ['Table', 'FoldMarker', 'DepthLimitFoldMarker',
           'EntryLimitFoldMarker', 'SharedReferenceFoldMarker',
           'CycleFoldMarker']


def to_text(obj, maxlen=None):
    try:
        text = str(obj)
    except Exception:
        try:
            text = str(repr(obj))
        except Exception:
            text = str(object.__repr__(obj))
    if maxlen and len(text) > maxlen:
        text = text[:maxlen - 3] + '...'
        # TODO: inverse of ljust/rjust/center
    return text


def escape_html(obj, maxlen=None):
    text = to_text(obj, maxlen=maxlen)
    return html_escape(text, quote=True)


_DNR = {type(None), bool, complex, float, type(NotImplemented), slice,
        str, bytes, int,
        types.FunctionType, types.MethodType,
        types.BuiltinFunctionType, types.GeneratorType}


# distinct, non-string labels so a fold marker can never be mistaken for a
# real cell value, no matter what the underlying data happens to contain
_FOLD_KIND_DEPTH = 'depth-limit'
_FOLD_KIND_ENTRIES = 'entry-limit'
_FOLD_KIND_SHARED = 'shared-reference'
_FOLD_KIND_CYCLE = 'cycle'

# header paired with the trailing cell that holds an entry-limit marker, so
# headed tables keep their columns aligned after folding
_FOLD_HEADER = '<fold>'


class FoldMarker:
    """A typed placeholder standing in for data that a :meth:`Table.from_data`
    preview walk could not or was not allowed to render.

    Fold markers are never plain strings: renderers detect them with
    :func:`isinstance`, so truncated preview text can never be confused
    with an actual cell value. Every marker carries the *type_name* of the
    folded object and the *path* at which it was reached, relative to the
    root of the data handed to :meth:`Table.from_data`.

    The four subclasses distinguish the four fold reasons:

    * :class:`DepthLimitFoldMarker` - nesting reached ``max_depth``
    * :class:`EntryLimitFoldMarker` - a container exceeded ``max_entries``
    * :class:`SharedReferenceFoldMarker` - the same object seen again at a
      non-ancestral path (first occurrence is rendered in full)
    * :class:`CycleFoldMarker` - the object is an ancestor of itself

    Both the HTML and text renderers turn the same marker objects into text,
    so both formats follow the exact same preview plan.
    """

    kind = 'fold'

    def __init__(self, type_name, path):
        self.type_name = type_name
        self.path = path

    @property
    def path_text(self):
        return _format_path(self.path)

    def __str__(self):
        return ('<fold:%s %s at %s>'
                % (self.kind, self.type_name, self.path_text))

    def __repr__(self):
        return ('%s(type_name=%r, path=%r)'
                % (self.__class__.__name__, self.type_name, self.path))

    def __eq__(self, other):
        if not isinstance(other, FoldMarker):
            return NotImplemented
        return (self.kind == other.kind
                and self.type_name == other.type_name
                and self.path == other.path)

    def __hash__(self):
        return hash((self.kind, self.type_name, tuple(self.path)))


class DepthLimitFoldMarker(FoldMarker):
    """A container left unrendered because expanding it would exceed the
    configured ``max_depth``."""
    kind = _FOLD_KIND_DEPTH


class EntryLimitFoldMarker(FoldMarker):
    """The tail of a container removed to respect the per-container
    ``max_entries`` budget. *omitted* is the number of hidden entries."""
    kind = _FOLD_KIND_ENTRIES

    def __init__(self, type_name, path, omitted):
        super().__init__(type_name, path)
        self.omitted = omitted

    def __str__(self):
        return ('<fold:%s %s at %s: %d more %s>'
                % (self.kind, self.type_name, self.path_text, self.omitted,
                   'entry' if self.omitted == 1 else 'entries'))

    def __repr__(self):
        return ('%s(type_name=%r, path=%r, omitted=%r)'
                % (self.__class__.__name__, self.type_name, self.path,
                   self.omitted))


class SharedReferenceFoldMarker(FoldMarker):
    """The same object encountered a second (or later) time at a
    non-ancestral path. The first occurrence is rendered in full and this
    marker points back at *first_path*."""
    kind = _FOLD_KIND_SHARED

    def __init__(self, type_name, path, first_path):
        super().__init__(type_name, path)
        self.first_path = first_path

    def __str__(self):
        return ('<fold:%s %s at %s: same object as %s>'
                % (self.kind, self.type_name, self.path_text,
                   _format_path(self.first_path)))

    def __repr__(self):
        return ('%s(type_name=%r, path=%r, first_path=%r)'
                % (self.__class__.__name__, self.type_name, self.path,
                   self.first_path))


class CycleFoldMarker(FoldMarker):
    """An object that is an ancestor of itself (a reference cycle). The
    marker points back at *cycle_path*, the path where the same object first
    appears on the active ancestor chain."""
    kind = _FOLD_KIND_CYCLE

    def __init__(self, type_name, path, cycle_path):
        super().__init__(type_name, path)
        self.cycle_path = cycle_path

    def __str__(self):
        return ('<fold:%s %s at %s: back to %s>'
                % (self.kind, self.type_name, self.path_text,
                   _format_path(self.cycle_path)))

    def __repr__(self):
        return ('%s(type_name=%r, path=%r, cycle_path=%r)'
                % (self.__class__.__name__, self.type_name, self.path,
                   self.cycle_path))


def _format_path(path):
    # path is a tuple of mapping keys / sequence indexes / attribute names
    if not path:
        return '$'
    parts = ['$']
    for seg in path:
        if isinstance(seg, int) and not isinstance(seg, bool):
            parts.append('[%d]' % seg)
        elif isinstance(seg, str) and seg.isidentifier():
            parts.append('.' + seg)
        else:
            parts.append('[%r]' % (seg,))
    return ''.join(parts)


class _PreviewContext:
    """Shared state for a single :meth:`Table.from_data` preview walk.

    Holds the budgets plus cycle/shared-reference bookkeeping and a cache so
    that any one object's property is accessed at most a single time.
    """

    def __init__(self, table_cls, max_depth, max_entries):
        self.table_cls = table_cls
        self.max_depth = max_depth
        self.max_entries = max_entries
        # id(obj) -> (obj, first path where it was fully rendered)
        self.seen = {}
        # ids of the objects on the current ancestor chain
        self.active = set()
        # cache of per-object attribute lookups, so properties run once
        self._attr_cache = {}
        # every marker produced by the walk, in creation order
        self.markers = []

    def _record(self, marker):
        self.markers.append(marker)
        return marker

    def get_attrs(self, obj):
        obj_id = id(obj)
        cached = self._attr_cache.get(obj_id)
        if cached is not None:
            return cached
        attrs = {}
        for attr in dir(obj):
            # an object's __dict__ could technically have non-string keys
            try:
                val = getattr(obj, attr)
            except Exception:
                # seen on greenlet: `run` shows in dir() but raises
                # AttributeError. Also properties misbehave.
                continue
            if callable(val):
                continue
            attrs[attr] = val
        self._attr_cache[obj_id] = attrs
        return attrs

    def guess_headers(self, input_type, obj):
        """Mirror :meth:`InputType.guess_headers` while funneling generic
        object attribute access through the cache, so the same property is
        not re-invoked when the object is later extracted."""
        if isinstance(input_type, ObjectInputType):
            return list(self.get_attrs(obj).keys())
        return input_type.guess_headers(obj)

    def _input_type_for(self, obj):
        for it in self.table_cls._input_types:
            if it.check_type(obj):
                return it
        return None

    def is_container(self, obj):
        return type(obj) not in _DNR and self._input_type_for(obj) is not None

    def extract(self, input_type, obj, headers):
        """Return a fresh ``(headers, [segment, ...], [value, ...])``
        description of *obj* without mutating the caller's data. Attribute
        access is funneled through the cache so object properties fire once.
        """
        if isinstance(input_type, DictInputType):
            items = sorted(obj.items())
            segs = [k for k, _ in items]
            values = [v for _, v in items]
            return list(segs), segs, values
        if isinstance(input_type, NamedTupleInputType):
            segs = list(obj._fields)
            return list(segs), segs, [getattr(obj, h) for h in segs]
        if isinstance(input_type, TupleInputType):
            return None, list(range(len(obj))), list(obj)
        if isinstance(input_type, ListInputType):
            return None, list(range(len(obj))), list(obj)
        # generic object: reuse the cached attribute snapshot, falling back
        # to any explicitly supplied headers
        attrs = self.get_attrs(obj)
        if headers is _MISSING or headers is None:
            segs = [a for a in dir(obj) if a in attrs]
        else:
            segs = list(headers)
        values = [attrs.get(h) for h in segs]
        return list(segs), segs, values

    def build_cell(self, value, path, depth_remaining, fold_at_limit=True):
        if not self.is_container(value):
            return value
        type_name = type(value).__name__
        obj_id = id(value)
        t_path = tuple(path)
        if obj_id in self.active:
            return self._record(CycleFoldMarker(
                type_name, t_path, self.seen[obj_id][1]))
        if obj_id in self.seen:
            return self._record(SharedReferenceFoldMarker(
                type_name, t_path, self.seen[obj_id][1]))
        # at max_depth == 1 the historical behavior keeps top-level cells
        # scalar; only depth budgets that allow nesting fold their leaves
        if depth_remaining <= 1:
            if fold_at_limit:
                return self._record(DepthLimitFoldMarker(type_name, t_path))
            return self._guard_scalar(value, t_path)
        input_type = self._input_type_for(value)
        self.seen[obj_id] = (value, t_path)
        self.active.add(obj_id)
        try:
            return self._build_table(input_type, value, path,
                                     depth_remaining - 1,
                                     fold_at_limit=fold_at_limit)
        finally:
            self.active.discard(obj_id)

    def _guard_scalar(self, value, path):
        """Keep *value* as a raw scalar cell, but walk it just far enough to
        replace reference cycles (which could not render anyway) and later
        occurrences of shared objects with typed markers."""
        if not self.is_container(value):
            return value
        obj_id = id(value)
        type_name = type(value).__name__
        if obj_id in self.active:
            return self._record(CycleFoldMarker(
                type_name, path, self.seen[obj_id][1]))
        if obj_id in self.seen:
            return self._record(SharedReferenceFoldMarker(
                type_name, path, self.seen[obj_id][1]))
        input_type = self._input_type_for(value)
        self.seen[obj_id] = (value, path)
        self.active.add(obj_id)
        try:
            _, segments, values = self.extract(input_type, value, _MISSING)
            for seg, val in zip(segments, values):
                if self.is_container(val):
                    self._guard_scalar(val, path + (seg,))
        finally:
            self.active.discard(obj_id)
        return value

    def build_row(self, input_type, obj, path, depth_remaining,
                  headers=_MISSING, fold_at_limit=True):
        """Build one table row (and matching headers) from *obj*, applying
        the per-container entry budget and folding the omitted tail into a
        typed marker. Returns ``(headers, row)``; *headers* is ``None`` for
        headerless sequence inputs."""
        out_headers, segments, values = self.extract(input_type, obj,
                                                     headers)
        omitted = 0
        if self.max_entries is not None and len(segments) > self.max_entries:
            omitted = len(segments) - self.max_entries
            segments = segments[:self.max_entries]
            values = values[:self.max_entries]

        row = [self.build_cell(val, path + [seg], depth_remaining,
                               fold_at_limit=fold_at_limit)
               for seg, val in zip(segments, values)]
        if omitted:
            row.append(self._record(EntryLimitFoldMarker(
                type(obj).__name__, tuple(path), omitted)))
        return out_headers, row

    def _build_table(self, input_type, obj, path, depth_remaining,
                     fold_at_limit=True):
        headers, row = self.build_row(input_type, obj, path,
                                      depth_remaining,
                                      fold_at_limit=fold_at_limit)
        return self.table_cls([row], headers=headers)


class UnsupportedData(TypeError):
    pass


class InputType:
    def __init__(self, *a, **kw):
        pass

    def get_entry_seq(self, data_seq, headers):
        return [self.get_entry(entry, headers) for entry in data_seq]


class DictInputType(InputType):
    def check_type(self, obj):
        return isinstance(obj, Mapping)

    def guess_headers(self, obj):
        return sorted(obj.keys())

    def get_entry(self, obj, headers):
        return [obj.get(h) for h in headers]

    def get_entry_seq(self, obj, headers):
        return [[ci.get(h) for h in headers] for ci in obj]


class ObjectInputType(InputType):
    def check_type(self, obj):
        return type(obj) not in _DNR and hasattr(obj, '__class__')

    def guess_headers(self, obj):
        headers = []
        for attr in dir(obj):
            # an object's __dict__ could technically have non-string keys
            try:
                val = getattr(obj, attr)
            except Exception:
                # seen on greenlet: `run` shows in dir() but raises
                # AttributeError. Also properties misbehave.
                continue
            if callable(val):
                continue
            headers.append(attr)
        return headers

    def get_entry(self, obj, headers):
        values = []
        for h in headers:
            try:
                values.append(getattr(obj, h))
            except Exception:
                values.append(None)
        return values


# might be better to hardcode list support since it's so close to the
# core or might be better to make this the copy-style from_* importer
# and have the non-copy style be hardcoded in __init__
class ListInputType(InputType):
    def check_type(self, obj):
        return isinstance(obj, MutableSequence)

    def guess_headers(self, obj):
        return None

    def get_entry(self, obj, headers):
        return obj

    def get_entry_seq(self, obj_seq, headers):
        return obj_seq


class TupleInputType(InputType):
    def check_type(self, obj):
        return isinstance(obj, tuple)

    def guess_headers(self, obj):
        return None

    def get_entry(self, obj, headers):
        return list(obj)

    def get_entry_seq(self, obj_seq, headers):
        return [list(t) for t in obj_seq]


class NamedTupleInputType(InputType):
    def check_type(self, obj):
        return hasattr(obj, '_fields') and isinstance(obj, tuple)

    def guess_headers(self, obj):
        return list(obj._fields)

    def get_entry(self, obj, headers):
        return [getattr(obj, h, None) for h in headers]

    def get_entry_seq(self, obj_seq, headers):
        return [[getattr(obj, h, None) for h in headers] for obj in obj_seq]


class Table:
    """
    This Table class is meant to be simple, low-overhead, and extensible. Its
    most common use would be for translation between in-memory data
    structures and serialization formats, such as HTML and console-ready text.

    As such, it stores data in list-of-lists format, and *does not* copy
    lists passed in. It also reserves the right to modify those lists in a
    "filling" process, whereby short lists are extended to the width of
    the table (usually determined by number of headers). This greatly
    reduces overhead and processing/validation that would have to occur
    otherwise.

    General description of headers behavior:

    Headers describe the columns, but are not part of the data, however,
    if the *headers* argument is omitted, Table tries to infer header
    names from the data. It is possible to have a table with no headers,
    just pass in ``headers=None``.

    Supported inputs:

    * :class:`list` of :class:`list` objects
    * :class:`dict` (list/single)
    * :class:`object` (list/single)
    * :class:`collections.namedtuple` (list/single)
    * TODO: DB API cursor?
    * TODO: json

    Supported outputs:

    * HTML
    * Pretty text (also usable as GF Markdown)
    * TODO: CSV
    * TODO: json
    * TODO: json lines

    When :meth:`from_data` is given a nesting (``max_depth > 1``) or entry
    (``max_entries``) budget, complex nested values are previewed rather than
    expanded without limit. Anything left unrendered is replaced by a typed
    :class:`FoldMarker` cell recording the folded object's type and its
    original path - never by truncated text pretending to be a real value.
    Reference cycles use :class:`CycleFoldMarker`, while a shared object seen
    again at a non-ancestral path uses :class:`SharedReferenceFoldMarker`.
    The markers produced by a walk are also collected on the resulting
    table's ``fold_markers`` attribute; the HTML and text renderers consume
    those same marker objects, so both formats follow one preview plan.

    To minimize resident size, the Table data is stored as a list of lists.
    """

    # order definitely matters here
    _input_types = [DictInputType(), ListInputType(),
                    NamedTupleInputType(), TupleInputType(),
                    ObjectInputType()]

    _html_tr, _html_tr_close = '<tr>', '</tr>'
    _html_th, _html_th_close = '<th>', '</th>'
    _html_td, _html_td_close = '<td>', '</td>'
    _html_thead, _html_thead_close = '<thead>', '</thead>'
    _html_tbody, _html_tbody_close = '<tbody>', '</tbody>'

    # _html_tfoot, _html_tfoot_close = '<tfoot>', '</tfoot>'
    _html_table_tag, _html_table_tag_close = '<table>', '</table>'

    def __init__(self, data=None, headers=_MISSING, metadata=None):
        if headers is _MISSING:
            headers = []
            if data:
                headers, data = list(data[0]), islice(data, 1, None)
        self.headers = list(headers) if headers else []
        self.metadata = metadata or {}
        self._data = []
        self._width = 0
        # every FoldMarker reachable from this table's cells, in walk order,
        # so both renderers follow one preview plan
        self.fold_markers = []

        self.extend(data)

    def extend(self, data):
        """
        Append the given data to the end of the Table.
        """
        if not data:
            return
        self._data.extend(data)
        self._set_width(reset=not self.headers)
        self._fill()

    def _set_width(self, reset=False):
        if reset:
            self._width = 0
        if self._width:
            return
        if self.headers:
            self._width = len(self.headers)
            return
        self._width = max([len(d) for d in self._data], default=0)

    def _fill(self):
        width, filler = self._width, [None]
        if not width:
            return
        for d in self._data:
            rem = width - len(d)
            if rem > 0:
                d.extend(filler * rem)
        return

    @classmethod
    def from_dict(cls, data, headers=_MISSING, max_depth=1, max_entries=None,
                  metadata=None):
        """Create a Table from a :class:`dict`. Operates the same as
        :meth:`from_data`, but forces interpretation of the data as a
        Mapping.
        """
        return cls.from_data(data=data, headers=headers,
                             max_depth=max_depth, max_entries=max_entries,
                             _data_type=DictInputType(),
                             metadata=metadata)

    @classmethod
    def from_list(cls, data, headers=_MISSING, max_depth=1, max_entries=None,
                  metadata=None):
        """Create a Table from a :class:`list`. Operates the same as
        :meth:`from_data`, but forces the interpretation of the data
        as a Sequence.
        """
        return cls.from_data(data=data, headers=headers,
                             max_depth=max_depth, max_entries=max_entries,
                             _data_type=ListInputType(),
                             metadata=metadata)

    @classmethod
    def from_object(cls, data, headers=_MISSING, max_depth=1,
                    max_entries=None, metadata=None):
        """Create a Table from an :class:`object`. Operates the same as
        :meth:`from_data`, but forces the interpretation of the data
        as an object. May be useful for some :class:`dict` and
        :class:`list` subtypes.
        """
        return cls.from_data(data=data, headers=headers,
                             max_depth=max_depth, max_entries=max_entries,
                             _data_type=ObjectInputType(),
                             metadata=metadata)

    @classmethod
    def from_data(cls, data, headers=_MISSING, max_depth=1,
                  max_entries=None, **kwargs):

        """Create a Table from any supported data, heuristically
        selecting how to represent the data in Table format.

        Args:
            data (object): Any object or iterable with data to be
                imported to the Table.

            headers (iterable): An iterable of headers to be matched
                to the data. If not explicitly passed, headers will be
                guessed for certain datatypes.

            max_depth (int): The level to which nested Tables should
                be created (default: 1). A container reached at the
                limit is not expanded: a
                :class:`DepthLimitFoldMarker` cell is emitted instead,
                carrying the container type and the path at which it
                was reached.

            max_entries (int): Per-container entry budget. When a
                mapping, sequence or object exposes more than this
                many entries, only the first *max_entries* are kept
                and an :class:`EntryLimitFoldMarker` records the
                number omitted. ``None`` (the default) means unlimited,
                which preserves the historical behavior.

            _data_type (InputType subclass): For advanced use cases,
                do not guess the type of the input data, use this data
                type instead.

        The input data is never modified. Reference cycles produce
        :class:`CycleFoldMarker` cells and the second (and later)
        occurrence of a shared object produces
        :class:`SharedReferenceFoldMarker` cells; both markers carry the
        original path of the data they stand in for. The HTML and text
        renderers consume the very same marker objects, so both formats
        follow one preview plan, and any one object property is accessed
        at most a single time during the walk.
        """
        # maxdepth follows the same behavior as find command
        # i.e., it doesn't work if max_depth=0 is passed in
        metadata = kwargs.pop('metadata', None)
        _data_type = kwargs.pop('_data_type', None)
        if max_entries is not None and (
                not isinstance(max_entries, int)
                or isinstance(max_entries, bool) or max_entries < 1):
            raise ValueError('max_entries must be a positive int or None,'
                             ' not %r' % (max_entries,))
        if headers is not _MISSING and headers is not None \
                and not isinstance(headers, str):
            headers = list(headers)

        if max_depth < 1:
            # return data instead?
            return cls(headers=headers, metadata=metadata)
        is_seq = isinstance(data, Sequence)
        if is_seq:
            if not data:
                return cls(headers=headers, metadata=metadata)
            to_check = data[0]
            if not _data_type:
                for it in cls._input_types:
                    if it.check_type(to_check):
                        _data_type = it
                        break
                else:
                    # not particularly happy about this rewind-y approach
                    is_seq = False
                    to_check = data
        else:
            if type(data) in _DNR:
                # hmm, got scalar data.
                # raise an exception or make an exception, nahmsayn?
                return cls([[data]], headers=headers, metadata=metadata)
            to_check = data
        if not _data_type:
            for it in cls._input_types:
                if it.check_type(to_check):
                    _data_type = it
                    break
            else:
                raise UnsupportedData('unsupported data type %r'
                                      % type(data))
        # The bounded preview walk is only enabled when a nesting or entry
        # budget is in play. Defaults (max_depth=1, max_entries=None) keep
        # the exact historical, copy-light behavior.
        bounded = max_depth > 1 or max_entries is not None
        ctx = _PreviewContext(cls, max_depth, max_entries)
        if headers is _MISSING:
            # guess through the same context the walk will use, so object
            # properties touched while guessing are not re-invoked later
            guessed_headers = ctx.guess_headers(_data_type, to_check)
        else:
            guessed_headers = headers
        if bounded:
            # top-level cells stay scalar when nesting is disabled
            fold_at_limit = max_depth > 1
            if is_seq:
                entries = []
                out_headers = guessed_headers
                for idx, item in enumerate(data):
                    out_headers, row = ctx.build_row(
                        _data_type, item, [idx], max_depth,
                        headers=guessed_headers,
                        fold_at_limit=fold_at_limit)
                    entries.append(row)
            else:
                out_headers, row = ctx.build_row(
                    _data_type, data, [], max_depth,
                    headers=guessed_headers,
                    fold_at_limit=fold_at_limit)
                entries = [row]
            # fold markers can leave rows wider than the guessed headers;
            # align everything to one width without touching the input
            width = max((len(r) for r in entries), default=0)
            if out_headers is not None and len(out_headers) < width:
                pad = width - len(out_headers)
                out_headers = (list(out_headers)
                               + [_FOLD_HEADER] + [None] * (pad - 1))
            for row in entries:
                if len(row) < width:
                    row.extend([None] * (width - len(row)))
            table = cls(entries, headers=out_headers, metadata=metadata)
            table.fold_markers = list(ctx.markers)
            return table

        # legacy path: still copy up front so the caller's input is never
        # mutated by the fill process
        if is_seq:
            raw_entries = _data_type.get_entry_seq(data, guessed_headers)
        else:
            raw_entries = [_data_type.get_entry(data, guessed_headers)]
        entries = [list(entry) for entry in raw_entries]
        if max_depth > 1:
            new_max_depth = max_depth - 1
            for i, entry in enumerate(entries):
                for j, cell in enumerate(entry):
                    if type(cell) in _DNR:
                        # optimization to avoid function overhead
                        continue
                    try:
                        entries[i][j] = cls.from_data(cell,
                                                      max_depth=new_max_depth)
                    except UnsupportedData:
                        continue
        return cls(entries, headers=guessed_headers, metadata=metadata)

    def __len__(self):
        return len(self._data)

    def __getitem__(self, idx):
        return self._data[idx]

    def __repr__(self):
        cn = self.__class__.__name__
        if self.headers:
            return f'{cn}(headers={self.headers!r}, data={self._data!r})'
        else:
            return f'{cn}({self._data!r})'

    def to_html(self, orientation=None, wrapped=True,
                with_headers=True, with_newlines=True,
                with_metadata=False, max_depth=1):
        """Render this Table to HTML. Configure the structure of Table
        HTML by subclassing and overriding ``_html_*`` class
        attributes.

        Args:
            orientation (str): one of 'auto', 'horizontal', or
                'vertical' (or the first letter of any of
                those). Default 'auto'.
            wrapped (bool): whether or not to include the wrapping
                '<table></table>' tags. Default ``True``, set to
                ``False`` if appending multiple Table outputs or an
                otherwise customized HTML wrapping tag is needed.
            with_newlines (bool): Set to ``True`` if output should
                include added newlines to make the HTML more
                readable. Default ``False``.
            with_metadata (bool/str): Set to ``True`` if output should
                be preceded with a Table of preset metadata, if it
                exists. Set to special value ``'bottom'`` if the
                metadata Table HTML should come *after* the main HTML output.
            max_depth (int): Indicate how deeply to nest HTML tables
                before simply reverting to :func:`repr`-ing the nested
                data.

        Returns:
            A text string of the HTML of the rendered table.

        """
        lines = []
        headers = []
        if with_metadata and self.metadata:
            metadata_table = Table.from_data(self.metadata,
                                             max_depth=max_depth)
            metadata_html = metadata_table.to_html(with_headers=True,
                                                   with_newlines=with_newlines,
                                                   with_metadata=False,
                                                   max_depth=max_depth)
            if with_metadata != 'bottom':
                lines.append(metadata_html)
                lines.append('<br />')

        if with_headers and self.headers:
            headers.extend(self.headers)
            headers.extend([None] * (self._width - len(self.headers)))
        if wrapped:
            lines.append(self._html_table_tag)
        orientation = orientation or 'auto'
        ol = orientation[0].lower()
        if ol == 'a':
            ol = 'h' if len(self) > 1 else 'v'
        if ol == 'h':
            self._add_horizontal_html_lines(lines, headers=headers,
                                            max_depth=max_depth)
        elif ol == 'v':
            self._add_vertical_html_lines(lines, headers=headers,
                                          max_depth=max_depth)
        else:
            raise ValueError("expected one of 'auto', 'vertical', or"
                             " 'horizontal', not %r" % orientation)
        if wrapped:
            lines.append(self._html_table_tag_close)
        if with_metadata and self.metadata and with_metadata == 'bottom':
            lines.append('<br />')
            lines.append(metadata_html)
        sep = '\n' if with_newlines else ''
        return sep.join(lines)

    def get_cell_html(self, value):
        """Called on each value in an HTML table. By default it simply escapes
        the HTML. Override this method to add additional conditions
        and behaviors, but take care to ensure the final output is
        HTML escaped.

        :class:`FoldMarker` cells are rendered from the marker's own
        typed text (the same text used by :meth:`to_text`), wrapped in
        a ``<span class="...">`` so preview folds stay distinguishable
        from real cell values.
        """
        if isinstance(value, FoldMarker):
            return ('<span class="fold-marker fold-%s">%s</span>'
                    % (value.kind, html_escape(str(value), quote=True)))
        return escape_html(value)

    def _add_horizontal_html_lines(self, lines, headers, max_depth):
        esc = self.get_cell_html
        new_depth = max_depth - 1 if max_depth > 1 else max_depth
        if max_depth > 1:
            new_depth = max_depth - 1
        if headers:
            _thth = self._html_th_close + self._html_th
            lines.append(self._html_thead)
            lines.append(self._html_tr + self._html_th +
                         _thth.join([esc(h) for h in headers]) +
                         self._html_th_close + self._html_tr_close)
            lines.append(self._html_thead_close)
        trtd, _tdtd, _td_tr = (self._html_tr + self._html_td,
                               self._html_td_close + self._html_td,
                               self._html_td_close + self._html_tr_close)
        lines.append(self._html_tbody)
        for row in self._data:
            if max_depth > 1:
                _fill_parts = []
                for cell in row:
                    if isinstance(cell, Table):
                        _fill_parts.append(cell.to_html(max_depth=new_depth))
                    else:
                        _fill_parts.append(esc(cell))
            else:
                _fill_parts = [esc(c) for c in row]
            lines.append(''.join([trtd, _tdtd.join(_fill_parts), _td_tr]))
        lines.append(self._html_tbody_close)

    def _add_vertical_html_lines(self, lines, headers, max_depth):
        esc = self.get_cell_html
        new_depth = max_depth - 1 if max_depth > 1 else max_depth
        tr, th, _th = self._html_tr, self._html_th, self._html_th_close
        td, _tdtd = self._html_td, self._html_td_close + self._html_td
        _td_tr = self._html_td_close + self._html_tr_close
        for i in range(self._width):
            line_parts = [tr]
            if headers:
                line_parts.extend([th, esc(headers[i]), _th])
            if max_depth > 1:
                new_depth = max_depth - 1
                _fill_parts = []
                for row in self._data:
                    cell = row[i]
                    if isinstance(cell, Table):
                        _fill_parts.append(cell.to_html(max_depth=new_depth))
                    else:
                        _fill_parts.append(esc(row[i]))
            else:
                _fill_parts = [esc(row[i]) for row in self._data]
            line_parts.extend([td, _tdtd.join(_fill_parts), _td_tr])
            lines.append(''.join(line_parts))

    def to_text(self, with_headers=True, maxlen=None):
        """Get the Table's textual representation. Nested tables are
        rendered recursively (without repeating their headers) so that
        :class:`FoldMarker` cells appear in text output exactly as they
        do in the HTML output, following the same preview plan.

        Args:
            with_headers (bool): Whether to include a header row at the top.
            maxlen (int): Max length of data in each cell.
        """
        lines = []
        widths = []
        headers = list(self.headers)
        if with_headers and headers:
            headers.extend([None] * (self._width - len(headers)))
        else:
            headers = []
        text_headers = [to_text(h, maxlen=maxlen) for h in headers]
        text_data = [[self._cell_to_text(cell, maxlen=maxlen)
                      for cell in row] for row in self._data]
        for idx in range(self._width):
            cur_widths = [len(row[idx]) for row in text_data]
            if text_headers:
                cur_widths.append(len(text_headers[idx]))
            widths.append(max(cur_widths, default=0))
        if text_headers:
            lines.append(' | '.join([h.center(widths[i])
                                     for i, h in enumerate(text_headers)]))
            lines.append('-|-'.join(['-' * w for w in widths]))
        for row in text_data:
            lines.append(' | '.join([cell.center(widths[j])
                                     for j, cell in enumerate(row)]))
        return '\n'.join(lines)

    def _cell_to_text(self, cell, maxlen=None):
        if isinstance(cell, FoldMarker):
            # markers render from their own typed text, never via truncation
            return str(cell)
        if isinstance(cell, Table):
            # recurse headerless so nested columns stay inside one cell;
            # a nested table with several rows is joined with newlines
            return '\n'.join(
                ' | '.join(self._cell_to_text(c, maxlen=maxlen) for c in row)
                for row in cell._data)
        return to_text(cell, maxlen=maxlen)
