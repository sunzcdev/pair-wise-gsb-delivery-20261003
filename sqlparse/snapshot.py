#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Versioned, pure-data snapshots of grouped token trees.

This module is a safe alternative to :mod:`pickle` for persisting the
result of sqlparse's grouping stage for offline review.  A snapshot is
a plain dictionary (JSON-serializable) that records, for every node of
the tree, the token type, the original value, the group class and the
ordered child nodes.

Restoring a snapshot never performs dynamic imports and never
instantiates classes outside a fixed whitelist of built-in group
classes.  Loading rejects

* unknown snapshot versions,
* unknown group classes,
* structurally invalid nodes (e.g. leaf tokens with children,
  ``Statement`` groups nested inside other groups),
* group values that do not match the concatenation of their children,
* inputs that exceed the depth or node-count limits.

Any violation raises :class:`SnapshotError`.
"""

import json

from sqlparse import sql
from sqlparse import tokens as T
from sqlparse.exceptions import SQLParseError

#: Version of the snapshot format produced and accepted by this module.
SNAPSHOT_VERSION = 1

#: Maximum nesting depth accepted when restoring a snapshot.  Keeps the
#: recursive loader well below the interpreter recursion limit.
MAX_SNAPSHOT_DEPTH = 100

#: Maximum number of nodes accepted when restoring a snapshot.
MAX_SNAPSHOT_NODES = 100000

#: Whitelist of built-in group classes that may appear in a snapshot.
#: Classes are looked up by name only; no dynamic imports take place.
_GROUP_CLASSES = {
    cls.__name__: cls
    for cls in (
        sql.TokenList,
        sql.Statement,
        sql.Identifier,
        sql.IdentifierList,
        sql.TypedLiteral,
        sql.Parenthesis,
        sql.SquareBrackets,
        sql.Assignment,
        sql.If,
        sql.For,
        sql.Comparison,
        sql.Comment,
        sql.Where,
        sql.Over,
        sql.Having,
        sql.Case,
        sql.Function,
        sql.Begin,
        sql.Operation,
        sql.Values,
        sql.Command,
    )
}

#: Group classes that may only appear as the root of a snapshot.
_ROOT_ONLY_CLASSES = frozenset({'Statement'})

_TOKEN_KEYS = frozenset({'type', 'ttype', 'value'})
_GROUP_KEYS = frozenset({'type', 'class', 'value', 'children'})
_DOCUMENT_KEYS = frozenset({'version', 'tree'})


class SnapshotError(SQLParseError):
    """Raised when a snapshot cannot be created or restored."""


def _dump_token(token):
    if token.is_group:
        cls_name = type(token).__name__
        if cls_name not in _GROUP_CLASSES:
            raise SnapshotError(
                f'Cannot snapshot unsupported group class {cls_name!r}.')
        children = [_dump_token(child) for child in token.tokens]
        if token.value != ''.join(child['value'] for child in children):
            raise SnapshotError(
                f'Group value {token.value!r} does not match the '
                'concatenation of its children.')
        return {
            'type': 'group',
            'class': cls_name,
            'value': token.value,
            'children': children,
        }
    if not token.ttype:
        raise SnapshotError('Cannot snapshot a token without a token type.')
    return {
        'type': 'token',
        'ttype': '.'.join(token.ttype),
        'value': token.value,
    }


def dump(tokenlist):
    """Return a versioned pure-data snapshot of *tokenlist*.

    *tokenlist* must be a :class:`~sqlparse.sql.TokenList` (usually a
    :class:`~sqlparse.sql.Statement` produced by :func:`sqlparse.parse`).
    The returned dictionary only contains built-in types (dicts, lists,
    strings and integers) and can be serialized with :mod:`json`.
    """
    if not isinstance(tokenlist, sql.TokenList):
        raise SnapshotError(
            'Can only snapshot TokenList instances, '
            f'got {type(tokenlist).__name__!r}.')
    return {'version': SNAPSHOT_VERSION, 'tree': _dump_token(tokenlist)}


def dumps(tokenlist, **json_kwargs):
    """Return *tokenlist* as a JSON snapshot string."""
    return json.dumps(dump(tokenlist), **json_kwargs)


def _resolve_ttype(path):
    """Resolve a dotted token type path against ``sqlparse.tokens.Token``.

    Only simple identifier segments are accepted, so resolution never
    escapes the token type hierarchy and never triggers imports.
    """
    if not isinstance(path, str) or not path:
        raise SnapshotError(f'Invalid token type {path!r}.')
    ttype = T.Token
    for segment in path.split('.'):
        if not segment.isidentifier() or segment.startswith('_'):
            raise SnapshotError(f'Invalid token type {path!r}.')
        ttype = getattr(ttype, segment)
    return ttype


class _Builder:
    """Validating builder that turns snapshot data back into tokens."""

    def __init__(self):
        self._nodes = 0

    def build(self, node, depth=0, is_root=False):
        self._nodes += 1
        if self._nodes > MAX_SNAPSHOT_NODES:
            raise SnapshotError(
                f'Snapshot exceeds the node limit ({MAX_SNAPSHOT_NODES}).')
        if depth > MAX_SNAPSHOT_DEPTH:
            raise SnapshotError(
                f'Snapshot exceeds the depth limit ({MAX_SNAPSHOT_DEPTH}).')
        if not isinstance(node, dict):
            raise SnapshotError(
                f'Snapshot node must be a mapping, got {type(node).__name__!r}.')

        node_type = node.get('type')
        if node_type == 'token':
            return self._build_token(node)
        elif node_type == 'group':
            return self._build_group(node, depth, is_root)
        raise SnapshotError(f'Unknown snapshot node type {node_type!r}.')

    def _build_token(self, node):
        if set(node) != _TOKEN_KEYS:
            raise SnapshotError(
                f'Malformed token node with keys {sorted(node)!r}.')
        value = node['value']
        if not isinstance(value, str):
            raise SnapshotError('Token value must be a string.')
        return sql.Token(_resolve_ttype(node['ttype']), value)

    def _build_group(self, node, depth, is_root):
        if set(node) != _GROUP_KEYS:
            raise SnapshotError(
                f'Malformed group node with keys {sorted(node)!r}.')
        cls_name = node['class']
        if not isinstance(cls_name, str) or cls_name not in _GROUP_CLASSES:
            raise SnapshotError(f'Unknown group class {cls_name!r}.')
        if cls_name in _ROOT_ONLY_CLASSES and not is_root:
            raise SnapshotError(
                f'Group class {cls_name!r} is only allowed at the root.')
        value = node['value']
        if not isinstance(value, str):
            raise SnapshotError('Group value must be a string.')
        children = node['children']
        if not isinstance(children, list):
            raise SnapshotError('Group children must be a list.')

        tokens = [self.build(child, depth + 1) for child in children]
        if value != ''.join(token.value for token in tokens):
            raise SnapshotError(
                f'Group value {value!r} does not match the concatenation '
                'of its children.')
        return _GROUP_CLASSES[cls_name](tokens)


def load(data):
    """Restore a token tree from a snapshot produced by :func:`dump`.

    Raises :class:`SnapshotError` if *data* is not a supported, well
    formed snapshot.
    """
    if not isinstance(data, dict) or set(data) != _DOCUMENT_KEYS:
        raise SnapshotError(
            'Snapshot must be a mapping with keys "version" and "tree".')
    version = data['version']
    if not isinstance(version, int) or isinstance(version, bool) \
            or version != SNAPSHOT_VERSION:
        raise SnapshotError(f'Unsupported snapshot version {version!r}.')

    root = _Builder().build(data['tree'], is_root=True)
    if not isinstance(root, sql.TokenList):
        raise SnapshotError('Snapshot root must be a group.')
    return root


def loads(s):
    """Restore a token tree from a JSON snapshot string."""
    try:
        data = json.loads(s)
    except (TypeError, ValueError, RecursionError) as exc:
        raise SnapshotError(f'Invalid snapshot document: {exc}') from exc
    return load(data)
