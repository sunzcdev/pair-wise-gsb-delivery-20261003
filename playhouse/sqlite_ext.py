import json
import re

from peewee import *
from peewee import ColumnBase
from peewee import EnclosedNodeList
from peewee import Entity
from peewee import Expression
from peewee import Node
from peewee import NodeList
from peewee import OP
from peewee import merge_dict
from peewee import sqlite3
from playhouse import fts_parser as _fts_parser


FTS3_MATCHINFO = 'pcx'
FTS4_MATCHINFO = 'pcnalx'
FTS_VERSION = 4
FTS5_MIN_SQLITE_VERSION = (3, 9, 0)


class RowIDField(AutoField):
    auto_increment = True
    column_name = name = required_name = 'rowid'

    def bind(self, model, name, *args):
        if name != self.required_name:
            raise ValueError('%s must be named "%s".' %
                             (type(self), self.required_name))
        super(RowIDField, self).bind(model, name, *args)


class DocIDField(RowIDField):
    column_name = name = required_name = 'docid'


class AutoIncrementField(AutoField):
    def ddl(self, ctx):
        node_list = super(AutoIncrementField, self).ddl(ctx)
        return NodeList((node_list, SQL('AUTOINCREMENT')))


class TDecimalField(DecimalField):
    field_type = 'TEXT'
    def get_modifiers(self): pass

    def db_value(self, value):
        if value is not None:
            return str(super(TDecimalField, self).db_value(value))


class ISODateTimeField(DateTimeField):
    formats = [
        '%Y-%m-%dT%H:%M:%S.%f%z',
        '%Y-%m-%dT%H:%M:%S%z',
        '%Y-%m-%dT%H:%M:%S.%f',
        '%Y-%m-%dT%H:%M:%S',
        '%Y-%m-%d',
    ]

    def db_value(self, value):
        if isinstance(value, str):
            # Normalize string input so the column stays uniformly ISO.
            value = self.adapt(value)
        if value and not isinstance(value, str):
            return value.isoformat()
        return value


class JSONPathSegment(object):
    """
    Base-class for the structural components of a SQLite JSON path.

    A path is a sequence of segments: object keys, array indexes and the
    tail-append marker. Path strings are generated from the segments, never
    by string-concatenation at the call-site, so keys containing dots,
    quotes, backslashes or control characters are escaped correctly and
    cannot be confused with path syntax.
    """
    __slots__ = ()

    def json_path(self):
        """Return this segment's path fragment, e.g. ``."key"``."""
        raise NotImplementedError

    @classmethod
    def coerce(cls, segment):
        # Shorthand used by ``__getitem__`` / ``path()``: strings are object
        # keys, integers are array indexes.
        if isinstance(segment, JSONPathSegment):
            return segment
        if isinstance(segment, bool):
            raise TypeError('JSON path segments cannot be booleans: %r.' %
                            (segment,))
        if isinstance(segment, str):
            return JSONPathKey(segment)
        if isinstance(segment, int):
            return JSONPathIndex(segment)
        raise TypeError('Unsupported JSON path segment: %r.' % (segment,))

class JSONPathKey(JSONPathSegment):
    """
    An object key. Rendered as a quoted JSON string, so keys holding dots,
    quotes, backslashes and other characters are unambiguous.
    """
    __slots__ = ('key',)

    def __init__(self, key):
        if not isinstance(key, str):
            raise TypeError('JSON object keys must be strings, got %r.' %
                            (key,))
        self.key = key

    def json_path(self):
        # json.dumps() applies the same escaping rules SQLite uses when
        # parsing a quoted path key (quotes, backslashes, control chars),
        # and escapes non-ASCII characters as \uXXXX.
        return '.%s' % json.dumps(self.key)

    def __eq__(self, other):
        return type(other) is JSONPathKey and other.key == self.key

    def __ne__(self, other):
        return not (self == other)

    def __hash__(self):
        return hash((JSONPathKey, self.key))

    def __repr__(self):
        return 'JSONPathKey(%r)' % (self.key,)

class JSONPathIndex(JSONPathSegment):
    """An array index. Negative indexes count from the end of the array."""
    __slots__ = ('index',)

    def __init__(self, index):
        if isinstance(index, bool) or not isinstance(index, int):
            raise TypeError('JSON array indexes must be integers, got %r.' %
                            (index,))
        self.index = index

    def json_path(self):
        if self.index < 0:
            # SQLite spells negative indexes relative to the array length.
            return '[#%d]' % self.index
        return '[%d]' % self.index

    def __eq__(self, other):
        return type(other) is JSONPathIndex and other.index == self.index

    def __ne__(self, other):
        return not (self == other)

    def __hash__(self):
        return hash((JSONPathIndex, self.index))

    def __repr__(self):
        return 'JSONPathIndex(%r)' % (self.index,)

class JSONPathAppend(JSONPathSegment):
    """
    The tail-append marker, addressing the position after the last element
    of an array.
    """
    __slots__ = ()

    def json_path(self):
        return '[#]'

    def __eq__(self, other):
        return type(other) is JSONPathAppend

    def __ne__(self, other):
        return not (self == other)

    def __hash__(self):
        return hash(JSONPathAppend)

    def __repr__(self):
        return 'JSONPathAppend()'

class JSONPathRaw(JSONPathSegment):
    """
    An already-formed JSON path, e.g. ``$.k1``. Retained for backwards
    compatibility with the legacy ``remove(*paths)`` API -- prefer the
    structured segments, which quote keys for you.
    """
    __slots__ = ('path',)

    def __init__(self, path):
        if not isinstance(path, str):
            raise TypeError('Raw JSON paths must be strings, got %r.' %
                            (path,))
        if not path.startswith('$'):
            raise ValueError('Raw JSON paths must begin with "$", got %r.' %
                             (path,))
        self.path = path

    def json_path(self):
        return self.path

    def __eq__(self, other):
        return type(other) is JSONPathRaw and other.path == self.path

    def __ne__(self, other):
        return not (self == other)

    def __hash__(self):
        return hash((JSONPathRaw, self.path))

    def __repr__(self):
        return 'JSONPathRaw(%r)' % (self.path,)


def _json_path_string(segments):
    # A raw path already carries the leading "$", so it has to stand alone:
    # it is absolute and cannot be prefixed or extended.
    if len(segments) == 1 and isinstance(segments[0], JSONPathRaw):
        return segments[0].json_path()
    for segment in segments:
        if isinstance(segment, JSONPathRaw):
            raise ValueError('A raw JSON path cannot be combined with other '
                             'path segments.')
    return '$' + ''.join([s.json_path() for s in segments])

def _json_path_value(segments):
    # Paths are always passed to the JSON1 functions as bound parameters, so
    # they are never interpreted as (or spliced into) the SQL statement.
    return Value(_json_path_string(segments), converter=False)


class JSONPath(ColumnBase):
    # Mutation hooks, overridden by JSONBPath to use the jsonb() functions.
    _json_wrap = staticmethod(fn.json)
    _json_set = staticmethod(fn.json_set)
    _json_insert = staticmethod(fn.json_insert)
    _json_replace = staticmethod(fn.json_replace)
    _json_remove = staticmethod(fn.json_remove)
    _json_patch = staticmethod(fn.json_patch)

    def __init__(self, field, segments=None):
        super(JSONPath, self).__init__()
        self._field = field
        self._segments = tuple(
            [JSONPathSegment.coerce(s) for s in (segments or ())])

    def _converter(self, value):
        return self._field.python_value(value)

    @property
    def segments(self):
        return self._segments

    @property
    def path(self):
        return _json_path_value(self._segments)

    def __getitem__(self, item):
        if isinstance(item, str) and item == '#':
            # Backwards-compatible shorthand for the tail-append marker. Use
            # key('#') to address an object key literally named "#".
            item = JSONPathAppend()
        return type(self)(self._field, self._segments + (item,))

    def extend(self, *segments):
        """Extend the path with one or more path segments."""
        return type(self)(self._field, self._segments + segments)

    def key(self, key):
        """Extend the path with an object-key segment."""
        return self.extend(JSONPathKey(key))

    def at(self, index):
        """Extend the path with an array-index segment."""
        return self.extend(JSONPathIndex(index))

    def tail(self):
        """Extend the path with a tail-append segment (``[#]``)."""
        return self.extend(JSONPathAppend())

    def _json_value(self, value, as_json=None):
        # Explicit JSON null (None), containers and booleans are serialized
        # and marked as JSON, so an explicit null is stored as JSON null
        # rather than nulling the column (or being read as SQL NULL), and
        # booleans are stored as true/false rather than the driver's 0/1.
        if isinstance(value, Node):
            return value
        if as_json or value is None or isinstance(value, (list, dict, bool)):
            return self._json_wrap(self._field._json_dumps(value))
        return value

    @staticmethod
    def _coerce_segments(path):
        if isinstance(path, JSONPath):
            return path.segments
        if isinstance(path, JSONPathSegment):
            return (path,)
        if isinstance(path, bool):
            raise TypeError('JSON path segments cannot be booleans: %r.' %
                            (path,))
        if isinstance(path, (str, int)):
            return (JSONPathSegment.coerce(path),)
        return tuple([JSONPathSegment.coerce(s) for s in path])

    def _path_value(self, path):
        return _json_path_value(
            self._segments + self._coerce_segments(path))

    def set(self, value, as_json=None):
        return self._json_set(self._field, self.path,
                              self._json_value(value, as_json))

    def insert(self, value, as_json=None):
        return self._json_insert(self._field, self.path,
                                 self._json_value(value, as_json))

    def replace(self, value, as_json=None):
        return self._json_replace(self._field, self.path,
                                  self._json_value(value, as_json))

    def update(self, value):
        return self.set(self._json_patch(self, self._field._json_dumps(value)))

    def remove(self):
        return self._json_remove(self._field, self.path)

    def append(self, value, as_json=None):
        return self._json_set(self._field, self.tail().path,
                              self._json_value(value, as_json))

    # Batch operations. Every path/value pair is handed to a single JSON1
    # function call, so the updates are applied in the order given, with each
    # update observing the result of the preceding one.
    def _many(self, func, updates, as_json=None):
        args = [self._field]
        for path, value in updates:
            args.append(self._path_value(path))
            args.append(self._json_value(value, as_json))
        return func(*args)

    def set_many(self, updates, as_json=None):
        """
        Apply multiple ``(path, value)`` updates with a single ``json_set()``.
        Pairs are applied in order and missing parents are created as needed.
        """
        return self._many(self._json_set, updates, as_json)

    def insert_many(self, updates, as_json=None):
        """Multiple path updates via a single ``json_insert()``. Paths that
        already exist are left untouched."""
        return self._many(self._json_insert, updates, as_json)

    def replace_many(self, updates, as_json=None):
        """Multiple path updates via a single ``json_replace()``. Paths that
        do not exist are left untouched."""
        return self._many(self._json_replace, updates, as_json)

    def remove_many(self, paths):
        """
        Remove multiple paths with a single ``json_remove()``. Removal is
        applied in order, so removing an array element shifts the indexes
        used by subsequent paths.
        """
        args = [self._field]
        for path in paths:
            args.append(self._path_value(path))
        return self._json_remove(*args)

    def json_type(self):
        return fn.json_type(self._field, self.path)

    def length(self):
        return fn.json_array_length(self._field, self.path)

    def children(self):
        return fn.json_each(self._field, self.path)

    def tree(self):
        return fn.json_tree(self._field, self.path)

    def __sql__(self, ctx):
        return ctx.sql(fn.json_extract(self._field, self.path)
                       if self._segments else self._field)

class JSONBPath(JSONPath):
    _json_wrap = staticmethod(fn.jsonb)
    _json_set = staticmethod(fn.jsonb_set)
    _json_insert = staticmethod(fn.jsonb_insert)
    _json_replace = staticmethod(fn.jsonb_replace)
    _json_remove = staticmethod(fn.jsonb_remove)
    _json_patch = staticmethod(fn.jsonb_patch)

    def __sql__(self, ctx):
        return ctx.sql(fn.jsonb_extract(self._field, self.path)
                       if self._segments else self._field)


class JSONField(TextField):
    field_type = 'TEXT'
    Path = JSONPath

    def __init__(self, json_dumps=None, json_loads=None, **kwargs):
        self._json_dumps = json_dumps or json.dumps
        self._json_loads = json_loads or json.loads
        super(JSONField, self).__init__(**kwargs)

    def python_value(self, value):
        if value is not None:
            try:
                return self._json_loads(value)
            except (TypeError, ValueError):
                return value

    def db_value(self, value):
        if value is not None:
            if not isinstance(value, Node):
                value = fn.json(self._json_dumps(value))
            return value

    def _e(op):
        def inner(self, rhs):
            if isinstance(rhs, (list, dict)):
                rhs = AsIs(rhs, self.db_value)
            return Expression(self, op, rhs)
        return inner
    __eq__ = _e(OP.EQ)
    __ne__ = _e(OP.NE)
    __gt__ = _e(OP.GT)
    __ge__ = _e(OP.GTE)
    __lt__ = _e(OP.LT)
    __le__ = _e(OP.LTE)
    __hash__ = Field.__hash__

    def __getitem__(self, item):
        return self.Path(self)[item]

    def path(self, *segments):
        """
        Return a :class:`JSONPath` built from structured segments. Strings
        are object keys, integers are array indexes, and
        :class:`JSONPathSegment` instances are used as-is. A single tuple or
        list argument is expanded, so a stored path can be passed through.

        .. code-block:: python

           Config.data.path('a.b', 0)                      # $."a.b"[0]
           Config.data.path(('a.b', 0))                    # same
           Config.data.path(JSONPathKey('a.b'), 0)         # same
        """
        if len(segments) == 1 and isinstance(segments[0], (list, tuple)):
            segments = tuple(segments[0])
        return self.Path(self, segments)

    def key(self, key):
        """Return a path rooted at an object key (however it is spelled)."""
        return self.Path(self).key(key)

    def at(self, index):
        """Return a path rooted at an array index."""
        return self.Path(self).at(index)

    def tail(self):
        """Return the tail-append path (``$[#]``)."""
        return self.Path(self).tail()

    def extract(self, *paths):
        paths = [Value(p, converter=False) for p in paths]
        return fn.json_extract(self, *paths)
    def extract_json(self, path):
        return Expression(self, '->', Value(path, converter=False))
    def extract_text(self, path):
        return Expression(self, '->>', Value(path, converter=False))

    def append(self, value, as_json=None):
        return self.Path(self).append(value, as_json)

    def insert(self, value, as_json=None):
        return self.Path(self).insert(value, as_json)

    def set(self, value, as_json=None):
        return self.Path(self).set(value, as_json)

    def replace(self, value, as_json=None):
        return self.Path(self).replace(value, as_json)

    def update(self, data):
        return self.Path(self).update(data)

    def remove(self, *paths):
        """
        Remove the paths from the document. Deletion is applied in order, so
        removing an array element shifts the indexes of later paths.

        Each path is a :class:`JSONPathSegment`, a :class:`JSONPath`, a tuple
        of segments, or the ``str`` / ``int`` shorthand (object key / array
        index). For backwards compatibility a string beginning with ``$`` is
        treated as an already-formed JSON path; use :class:`JSONPathKey` to
        address an object key that begins with ``$``.
        """
        path = self.Path(self)
        if not paths:
            return path.remove()
        return path.remove_many([self._remove_path(p) for p in paths])

    @staticmethod
    def _remove_path(path):
        if isinstance(path, str) and path.startswith('$'):
            return JSONPathRaw(path)
        return path

    def set_many(self, updates, as_json=None):
        """Apply multiple ``(path, value)`` updates in a single ``json_set()``.
        Updates are applied in the order given."""
        return self.Path(self).set_many(updates, as_json)

    def insert_many(self, updates, as_json=None):
        """Multiple ``(path, value)`` pairs via a single ``json_insert()``.
        Existing paths are not overwritten."""
        return self.Path(self).insert_many(updates, as_json)

    def replace_many(self, updates, as_json=None):
        """Multiple ``(path, value)`` pairs via a single ``json_replace()``.
        Non-existent paths are not created."""
        return self.Path(self).replace_many(updates, as_json)

    def remove_many(self, paths):
        """Remove multiple paths in a single ``json_remove()`` call. Unlike
        :meth:`remove`, every path is interpreted structurally (a string is an
        object key, even if it begins with ``$``)."""
        return self.Path(self).remove_many(paths)

    def json_type(self):
        return fn.json_type(self)

    def length(self, path=None):
        args = (self, path) if path else (self,)
        return fn.json_array_length(*args)

    def children(self):
        """
        Schema of `json_each` and `json_tree`:

        key,
        value,
        type TEXT (object, array, string, etc),
        atom (value for primitive/scalar types, NULL for array and object)
        id INTEGER (unique identifier for element)
        parent INTEGER (unique identifier of parent element or NULL)
        fullkey TEXT (full path describing element)
        path TEXT (path to the container of the current element)
        json JSON hidden (1st input parameter to function)
        root TEXT hidden (2nd input parameter, path at which to start)
        """
        return fn.json_each(self)

    def tree(self):
        return fn.json_tree(self)


class JSONBField(JSONField):
    field_type = 'JSONB'
    Path = JSONBPath

    def db_value(self, value):
        if value is not None:
            if not isinstance(value, Node):
                value = fn.jsonb(self._json_dumps(value))
            return value

    def json(self):
        return fn.json(self)

    def extract(self, *paths):
        paths = [Value(p, converter=False) for p in paths]
        return fn.jsonb_extract(self, *paths)


class SearchField(Field):
    def __init__(self, unindexed=False, column_name=None, **k):
        if k:
            raise ValueError('SearchField does not accept these keyword '
                             'arguments: %s.' % sorted(k))
        super(SearchField, self).__init__(unindexed=unindexed,
                                          column_name=column_name, null=True)

    def match(self, term):
        return match(self, term)

    @property
    def fts_column_index(self):
        if not hasattr(self, '_fts_column_index'):
            search_fields = [f.name for f in self.model._meta.sorted_fields
                             if isinstance(f, SearchField)]
            self._fts_column_index = search_fields.index(self.name)
        return self._fts_column_index

    def highlight(self, left, right):
        column_idx = self.fts_column_index
        return fn.highlight(self.model._meta.entity, column_idx, left, right)

    def snippet(self, left, right, over_length='...', max_tokens=16):
        if not (0 < max_tokens < 65):
            raise ValueError('max_tokens must be between 1 and 64 (inclusive)')
        column_idx = self.fts_column_index
        return fn.snippet(self.model._meta.entity, column_idx, left, right,
                          over_length, max_tokens)


class VirtualTableSchemaManager(SchemaManager):
    def _create_virtual_table(self, safe=True, **options):
        options = self.model.clean_options(
            merge_dict(self.model._meta.options, options))

        # Structure:
        # CREATE VIRTUAL TABLE <model>
        # USING <extension_module>
        # ([prefix_arguments, ...] fields, ... [arguments, ...], [options...])
        ctx = self._create_context()
        ctx.literal('CREATE VIRTUAL TABLE ')
        if safe:
            ctx.literal('IF NOT EXISTS ')
        (ctx
         .sql(self.model)
         .literal(' USING '))

        ext_module = self.model._meta.extension_module
        if isinstance(ext_module, Node):
            return ctx.sql(ext_module)

        ctx.sql(SQL(ext_module)).literal(' ')
        arguments = []
        meta = self.model._meta

        if meta.prefix_arguments:
            arguments.extend([SQL(a) for a in meta.prefix_arguments])

        # Constraints, data-types, foreign and primary keys are all omitted.
        for field in meta.sorted_fields:
            if isinstance(field, (RowIDField)) or field._hidden:
                continue
            field_def = [Entity(field.column_name)]
            if field.unindexed:
                field_def.append(SQL('UNINDEXED'))
            arguments.append(NodeList(field_def))

        if meta.arguments:
            arguments.extend([SQL(a) for a in meta.arguments])

        if options:
            arguments.extend(self._create_table_option_sql(options))
        return ctx.sql(EnclosedNodeList(arguments))

    def _create_table(self, safe=True, **options):
        if issubclass(self.model, VirtualModel):
            return self._create_virtual_table(safe, **options)

        return super(VirtualTableSchemaManager, self)._create_table(
            safe, **options)


class VirtualModel(Model):
    class Meta:
        arguments = None
        extension_module = None
        prefix_arguments = None
        primary_key = False
        schema_manager_class = VirtualTableSchemaManager

    @classmethod
    def clean_options(cls, options):
        return options


class BaseFTSModel(VirtualModel):
    @classmethod
    def clean_options(cls, options):
        content = options.get('content')
        prefix = options.get('prefix')
        tokenize = options.get('tokenize')

        if isinstance(content, str) and content == '':
            # Special-case content-less full-text search tables.
            options['content'] = "''"
        elif isinstance(content, Field):
            # A field generates DDL fts5 rejects and fts4 silently truncates.
            raise ImproperlyConfigured('The content option must be a Model, '
                                       'a table-name string, or the empty '
                                       'string (contentless).')

        if prefix:
            if isinstance(prefix, (list, tuple)):
                prefix = ','.join([str(i) for i in prefix])
            elif isinstance(prefix, int):
                prefix = str(prefix)
            options['prefix'] = "'%s'" % prefix.strip("' ")

        if tokenize and cls._meta.extension_module.lower() == 'fts5':
            # Tokenizers need to be in quoted string for FTS5, but not for FTS3
            # or FTS4.
            options['tokenize'] = '"%s"' % tokenize

        return options


class FTSModel(BaseFTSModel):
    """
    VirtualModel class for creating tables that use the FTS4 search
    extension. To use FTS3, set ``Meta.extension_module = 'FTS3'``.
    """
    rowid = RowIDField()

    class Meta:
        extension_module = 'FTS%s' % FTS_VERSION

    @classmethod
    def _fts_cmd(cls, cmd):
        tbl = cls._meta.table_name
        res = cls._meta.database.execute_sql(
            "INSERT INTO %s(%s) VALUES('%s');" % (tbl, tbl, cmd))
        return res.fetchone()

    @classmethod
    def optimize(cls):
        return cls._fts_cmd('optimize')

    @classmethod
    def rebuild(cls):
        return cls._fts_cmd('rebuild')

    @classmethod
    def integrity_check(cls):
        return cls._fts_cmd('integrity-check')

    @classmethod
    def merge(cls, blocks=200, segments=8):
        return cls._fts_cmd('merge=%s,%s' % (blocks, segments))

    @classmethod
    def automerge(cls, state=True):
        return cls._fts_cmd('automerge=%s' % (state and '1' or '0'))

    @classmethod
    def match(cls, term):
        """
        Generate a `MATCH` expression appropriate for searching this table.
        """
        return match(cls._meta.entity, term)

    @classmethod
    def rank(cls, *weights):
        matchinfo = fn.matchinfo(cls._meta.entity, FTS3_MATCHINFO)
        return fn.fts_rank(matchinfo, *weights)

    @classmethod
    def bm25(cls, *weights):
        match_info = fn.matchinfo(cls._meta.entity, FTS4_MATCHINFO)
        return fn.fts_bm25(match_info, *weights)

    @classmethod
    def bm25f(cls, *weights):
        match_info = fn.matchinfo(cls._meta.entity, FTS4_MATCHINFO)
        return fn.fts_bm25f(match_info, *weights)

    @classmethod
    def lucene(cls, *weights):
        match_info = fn.matchinfo(cls._meta.entity, FTS4_MATCHINFO)
        return fn.fts_lucene(match_info, *weights)

    @classmethod
    def _search(cls, term, weights, with_score, score_alias, score_fn,
                explicit_ordering):
        if not weights:
            rank = score_fn()
        elif isinstance(weights, dict):
            weight_args = []
            for field in cls._meta.sorted_fields:
                # rowid is not a searchable column.
                if isinstance(field, RowIDField):
                    continue
                # Attempt to get the specified weight of the field by looking
                # it up using it's field instance followed by name.
                field_weight = weights.get(field, weights.get(field.name, 1.0))
                weight_args.append(field_weight)
            rank = score_fn(*weight_args)
        else:
            rank = score_fn(*weights)

        selection = ()
        order_by = rank
        if with_score:
            selection = (cls, rank.alias(score_alias))
        if with_score and not explicit_ordering:
            order_by = SQL(score_alias)

        return (cls
                .select(*selection)
                .where(cls.match(term))
                .order_by(order_by))

    @classmethod
    def search(cls, term, weights=None, with_score=False, score_alias='score',
               explicit_ordering=False):
        """Full-text search using selected `term`."""
        return cls._search(
            term,
            weights,
            with_score,
            score_alias,
            cls.rank,
            explicit_ordering)

    @classmethod
    def search_bm25(cls, term, weights=None, with_score=False,
                    score_alias='score', explicit_ordering=False):
        """Full-text search for selected `term` using BM25 algorithm."""
        return cls._search(
            term,
            weights,
            with_score,
            score_alias,
            cls.bm25,
            explicit_ordering)

    @classmethod
    def search_bm25f(cls, term, weights=None, with_score=False,
                     score_alias='score', explicit_ordering=False):
        """Full-text search for selected `term` using BM25F algorithm."""
        return cls._search(
            term,
            weights,
            with_score,
            score_alias,
            cls.bm25f,
            explicit_ordering)

    @classmethod
    def search_lucene(cls, term, weights=None, with_score=False,
                      score_alias='score', explicit_ordering=False):
        """Full-text search for selected `term` using Lucene algorithm."""
        return cls._search(
            term,
            weights,
            with_score,
            score_alias,
            cls.lucene,
            explicit_ordering)


_alphabet = 'abcdefghijklmnopqrstuvwxyz'
_alphanum = (set('\t ,"(){}*:_+0123456789') |
             set(_alphabet) |
             set(_alphabet.upper()) |
             set((chr(26),)))
_invalid_ascii = set(chr(p) for p in range(128) if chr(p) not in _alphanum)
del _alphabet
del _alphanum
_quote_re = re.compile(r'[^\s"]+|"[^"\\]*(?:\\.[^"\\]*)*"')


class FTS5Model(BaseFTSModel):
    """
    Requires SQLite >= 3.9.0.

    Table options:

    content: table name of external content, or empty string for "contentless"
    content_rowid: column name of external content primary key
    prefix: integer(s). Ex: '2' or '2 3 4'
    tokenize: porter, unicode61, ascii. Ex: 'porter unicode61'

    The unicode tokenizer supports the following parameters:

    * remove_diacritics (1 or 0, default is 1)
    * tokenchars (string of characters, e.g. '-_'
    * separators (string of characters)

    Parameters are passed as alternating parameter name and value, so:

    {'tokenize': "unicode61 remove_diacritics 0 tokenchars '-_'"}

    Content-less tables:

    If you don't need the full-text content in its original form, you can
    specify a content-less table. Searches and auxiliary functions will work
    as usual, but the only values returned when SELECT-ing can be rowid. Also
    content-less tables do not support UPDATE or DELETE, unless created with
    the contentless_delete=1 option (sqlite >= 3.43).

    External content tables:

    You can set up triggers to sync these, e.g.

    -- Create a table. And an external content fts5 table to index it.
    CREATE TABLE tbl(a INTEGER PRIMARY KEY, b);
    CREATE VIRTUAL TABLE ft USING fts5(b, content='tbl', content_rowid='a');

    -- Triggers to keep the FTS index up to date.
    CREATE TRIGGER tbl_ai AFTER INSERT ON tbl BEGIN
      INSERT INTO ft(rowid, b) VALUES (new.a, new.b);
    END;
    CREATE TRIGGER tbl_ad AFTER DELETE ON tbl BEGIN
      INSERT INTO ft(ft, rowid, b) VALUES('delete', old.a, old.b);
    END;
    CREATE TRIGGER tbl_au AFTER UPDATE ON tbl BEGIN
      INSERT INTO ft(ft, rowid, b) VALUES('delete', old.a, old.b);
      INSERT INTO ft(rowid, b) VALUES (new.a, new.b);
    END;

    Note: with sqlite's default recursive_triggers=off, INSERT OR REPLACE on
    the content table skips the delete trigger and corrupts the index. Enable
    PRAGMA recursive_triggers if REPLACE may be used.

    Built-in auxiliary functions:

    * bm25(tbl[, weight_0, ... weight_n])
    * highlight(tbl, col_idx, prefix, suffix)
    * snippet(tbl, col_idx, prefix, suffix, ?, max_tokens)
    """
    # FTS5 does not support declared primary keys, but we can use the
    # implicit rowid.
    rowid = RowIDField()

    class Meta:
        extension_module = 'fts5'

    _error_messages = {
        'field_type': ('Besides the implicit `rowid` column, all columns must '
                       'be instances of SearchField'),
        'index': 'Secondary indexes are not supported for FTS5 models',
        'pk': 'FTS5 models must use the default `rowid` primary key',
    }

    @classmethod
    def validate_model(cls):
        # Perform FTS5-specific validation and options post-processing.
        if cls._meta.primary_key.name != 'rowid':
            raise ImproperlyConfigured(cls._error_messages['pk'])
        for field in cls._meta.fields.values():
            if not isinstance(field, (SearchField, RowIDField)):
                raise ImproperlyConfigured(cls._error_messages['field_type'])
        if cls._meta.indexes:
            raise ImproperlyConfigured(cls._error_messages['index'])

    @classmethod
    def fts5_installed(cls):
        if sqlite3.sqlite_version_info[:3] < FTS5_MIN_SQLITE_VERSION:
            return False

        # Test in-memory DB to determine if the FTS5 extension is installed.
        tmp_db = sqlite3.connect(':memory:')
        try:
            tmp_db.execute('CREATE VIRTUAL TABLE fts5test USING fts5 (data);')
        except:
            try:
                tmp_db.enable_load_extension(True)
                tmp_db.load_extension('fts5')
            except:
                return False
            else:
                cls._meta.database.load_extension('fts5')
        finally:
            tmp_db.close()

        return True

    @staticmethod
    def validate_query(query):
        """
        Simple helper function to indicate whether a search query is a
        valid FTS5 query. Note: this simply looks at the characters being
        used, and is not guaranteed to catch all problematic queries.
        """
        tokens = _quote_re.findall(query)
        for token in tokens:
            if token.startswith('"') and token.endswith('"'):
                continue
            if set(token) & _invalid_ascii:
                return False
        return True

    @staticmethod
    def clean_query(query, replace=chr(26)):
        """
        Clean a query of invalid tokens.
        """
        accum = []
        any_invalid = False
        tokens = _quote_re.findall(query)
        for token in tokens:
            if token.startswith('"') and token.endswith('"'):
                accum.append(token)
                continue
            token_set = set(token)
            invalid_for_token = token_set & _invalid_ascii
            if invalid_for_token:
                any_invalid = True
                for c in invalid_for_token:
                    token = token.replace(c, replace)
            accum.append(token)

        if any_invalid:
            return ' '.join(accum)
        return query

    @classmethod
    def web_query(cls, query):
        """
        Translate a search-box query (quoted phrases, AND/OR/NOT, -exclusion,
        column:filters, parentheses) into an FTS5 query.
        """
        columns = [f.column_name for f in cls._meta.sorted_fields
                   if isinstance(f, SearchField) and not f.unindexed]
        return _fts_parser.parse(query, columns)

    @classmethod
    def match(cls, term):
        """
        Generate a `MATCH` expression appropriate for searching this table.
        """
        return match(cls._meta.entity, term)

    @classmethod
    def rank(cls, *args):
        return cls.bm25(*args) if args else SQL('rank')

    @classmethod
    def bm25(cls, *weights):
        return fn.bm25(cls._meta.entity, *weights)

    @classmethod
    def search(cls, term, weights=None, with_score=False, score_alias='score',
               explicit_ordering=False):
        """Full-text search using selected `term`."""
        return cls.search_bm25(
            FTS5Model.clean_query(term),
            weights,
            with_score,
            score_alias,
            explicit_ordering)

    @classmethod
    def search_bm25(cls, term, weights=None, with_score=False,
                    score_alias='score', explicit_ordering=False):
        """Full-text search using selected `term`."""
        if not weights:
            rank = SQL('rank')
        elif isinstance(weights, dict):
            weight_args = []
            for field in cls._meta.sorted_fields:
                # bm25() assigns weights positionally across all columns,
                # including UNINDEXED columns, so they must not be skipped.
                if isinstance(field, SearchField):
                    weight_args.append(
                        weights.get(field, weights.get(field.name, 1.0)))
            rank = fn.bm25(cls._meta.entity, *weight_args)
        else:
            rank = fn.bm25(cls._meta.entity, *weights)

        selection = ()
        order_by = rank
        if with_score:
            selection = (cls, rank.alias(score_alias))
        if with_score and not explicit_ordering:
            order_by = SQL(score_alias)

        return (cls
                .select(*selection)
                .where(cls.match(FTS5Model.clean_query(term)))
                .order_by(order_by))

    @classmethod
    def _fts_cmd_sql(cls, cmd, **extra_params):
        tbl = cls._meta.entity
        columns = [tbl]
        values = [cmd]
        for key, value in extra_params.items():
            columns.append(Entity(key))
            values.append(value)

        return NodeList((
            SQL('INSERT INTO'),
            cls._meta.entity,
            EnclosedNodeList(columns),
            SQL('VALUES'),
            EnclosedNodeList(values)))

    @classmethod
    def _fts_cmd(cls, cmd, **extra_params):
        query = cls._fts_cmd_sql(cmd, **extra_params)
        return cls._meta.database.execute(query)

    @classmethod
    def automerge(cls, level):
        if not (0 <= level <= 64):
            raise ValueError('level must be between 0 and 64')
        return cls._fts_cmd('automerge', rank=level)

    @classmethod
    def merge(cls, npages):
        return cls._fts_cmd('merge', rank=npages)

    @classmethod
    def optimize(cls):
        return cls._fts_cmd('optimize')

    @classmethod
    def rebuild(cls):
        return cls._fts_cmd('rebuild')

    @classmethod
    def set_pgsz(cls, pgsz):
        return cls._fts_cmd('pgsz', rank=pgsz)

    @classmethod
    def set_rank(cls, rank_expression):
        return cls._fts_cmd('rank', rank=rank_expression)

    @classmethod
    def delete_all(cls):
        return cls._fts_cmd('delete-all')

    @classmethod
    def delete_command(cls, rowid, **values):
        """
        Remove a row using the fts5 'delete' command, passing the values
        exactly as they were indexed. This is how rows are removed from
        external-content and contentless tables, which cannot look up the
        old values themselves.
        """
        accum = {}
        for field in cls._meta.sorted_fields:
            if not isinstance(field, SearchField):
                continue
            if field.name in values:
                accum[field.column_name] = values.pop(field.name)
            elif field.column_name in values:
                accum[field.column_name] = values.pop(field.column_name)
            elif not field.unindexed:
                # Deleting against a missing value silently corrupts the
                # index, so all indexed columns are required.
                raise ValueError('delete_command() requires the value of "%s" '
                                 'as it was originally indexed.' % field.name)
        if values:
            raise ValueError('delete_command() received unrecognized column(s): '
                             '%s' % ', '.join(sorted(values)))
        return cls._fts_cmd('delete', rowid=rowid, **accum)

    @classmethod
    def integrity_check(cls, rank=0):
        return cls._fts_cmd('integrity-check', rank=rank)

    @classmethod
    def VocabModel(cls, table_type='row', table=None):
        if table_type not in ('row', 'col', 'instance'):
            raise ValueError('table_type must be either "row", "col" or '
                             '"instance".')

        # Each table-type needs a distinct default name, or the second type
        # silently binds to the first's table via IF NOT EXISTS.
        suffix = '_v' if table_type == 'row' else '_v_%s' % table_type

        class Meta:
            database = cls._meta.database
            table_name = table or cls._meta.table_name + suffix
            extension_module = fn.fts5vocab(
                cls._meta.entity,
                SQL(table_type))

        if table_type == 'row':
            columns = ('term', 'doc', 'cnt')
        elif table_type == 'col':
            columns = ('term', 'col', 'doc', 'cnt')
        else:
            columns = ('term', 'doc', 'col', 'offset')

        attrs = {c: TextField() if c in ('term', 'col') else IntegerField()
                 for c in columns}
        attrs['Meta'] = Meta

        # Built fresh per-call: caching would pin the database bound at the
        # time of the first call and ignore the table param thereafter.
        return type('%sVocab' % cls.__name__, (VirtualModel,), attrs)


OP.MATCH = 'MATCH'

def match(lhs, rhs):
    return Expression(lhs, OP.MATCH, rhs)
