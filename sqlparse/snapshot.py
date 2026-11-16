#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Versioned, pickle-free snapshots of grouped token trees.

The grouping results of :mod:`sqlparse` can be serialized as plain data
(nested dictionaries, lists and strings) so that offline reviews can store
them without relying on :mod:`pickle`.  A snapshot records, for every node,
the token type, the original value, the group category and the ordered
children of groups.

Only the built-in tree classes defined in :mod:`sqlparse.sql` can be
restored.  Loading is defensive:

* unknown snapshot versions and unknown group categories are rejected;
* malformed parent/child structures are rejected;
* inputs nested deeper than :data:`MAX_SNAPSHOT_DEPTH` are rejected;
* a group ``value`` must equal the concatenation of its subtree values.

No dynamic imports are performed; group classes are resolved through a
static registry only.
"""

from sqlparse import sql, tokens as T
from sqlparse.exceptions import SQLParseError

#: Version of the snapshot format produced and accepted by this module.
SNAPSHOT_VERSION = 1

#: Maximum accepted group nesting depth.  Mirrors the grouping depth limit
#: in :mod:`sqlparse.engine.grouping` so that snapshots of trees produced by
#: the parser always round-trip while hostile, deeply nested payloads fail.
MAX_SNAPSHOT_DEPTH = 100

#: Maximum number of nodes accepted in a single snapshot.  A purely
#: defensive bound against pathological allocations; valid parser output
#: stays well below it (grouping itself caps token lists at 10 000 tokens).
MAX_SNAPSHOT_NODES = 100_000

#: Maximum number of components of a serialized token type.  Token types
#: produced by the lexer are far shorter; the bound prevents pathological
#: ``_TokenType`` chain construction while loading untrusted data.
MAX_TTYPE_DEPTH = 32

# Static registry of restorable group categories.  Deliberately an explicit
# mapping instead of ``importlib``/``__import__`` lookups: untrusted data
# must never be able to trigger an import or instantiate arbitrary classes.
_GROUP_REGISTRY = {
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


def _ttype_to_path(ttype):
    """Return the plain-data path describing *ttype*."""
    if not isinstance(ttype, T._TokenType):
        raise SQLParseError(
            f"Cannot serialize token type {ttype!r}: not a _TokenType."
        )
    return list(ttype)


def _path_to_ttype(path):
    """Resolve a serialized token-type path against the static token tree."""
    if not isinstance(path, list) or not path:
        raise SQLParseError("Invalid token type: expected a non-empty list.")
    if len(path) > MAX_TTYPE_DEPTH:
        raise SQLParseError(
            f"Token type nesting too deep (limit {MAX_TTYPE_DEPTH})."
        )

    ttype = T.Token
    for component in path:
        # Only plain, non-dunder attribute names are resolved.  This keeps
        # restoration on the predefined token-type tree without ever
        # evaluating arbitrary attribute names.
        if not isinstance(component, str) or not component.isidentifier() \
                or component.startswith("__"):
            raise SQLParseError(f"Invalid token type component: {component!r}")
        ttype = getattr(ttype, component)

    return ttype


def tree_to_snapshot(token):
    """Serialize a grouped token tree to versioned pure data.

    :param token: a :class:`~sqlparse.sql.Token` or
        :class:`~sqlparse.sql.TokenList` instance.
    :returns: a versioned dictionary containing only dictionaries, lists
        and strings, suitable for JSON serialization.  Pass the result
        back to :func:`snapshot_to_tree` to restore the tree.
    """
    if not isinstance(token, sql.Token):
        raise SQLParseError(
            f"Cannot serialize object of type {type(token).__name__!r}."
        )

    if token.is_group:
        node = {
            "type": "group",
            "group": type(token).__name__,
            "value": token.value,
            "children": [tree_to_snapshot(child)["tree"]
                         for child in token.tokens],
        }
    else:
        node = {
            "type": "token",
            "ttype": _ttype_to_path(token.ttype),
            "value": token.value,
        }

    return {"version": SNAPSHOT_VERSION, "tree": node}


def _invalid(message):
    raise SQLParseError(f"Invalid snapshot: {message}")


def _validate_snapshot(snapshot):
    """Walk the raw snapshot once, validating structure and limits.

    Returns the validated root node and the total node count.  The walk is
    fully iterative so that deep payloads fail with a clean error instead of
    a :class:`RecursionError`, and the caller's snapshot mapping is never
    mutated.
    """
    if not isinstance(snapshot, dict):
        _invalid("top-level object must be a mapping.")
    if snapshot.get("version") != SNAPSHOT_VERSION:
        raise SQLParseError(
            f"Unsupported snapshot version: {snapshot.get('version')!r} "
            f"(supported: {SNAPSHOT_VERSION})."
        )

    root = snapshot.get("tree")
    if not isinstance(root, dict):
        _invalid("missing root 'tree' node.")

    seen_nodes = set()
    # Entries are (node, depth) where depth counts the enclosing groups.
    stack = [(root, 0)]
    node_count = 0

    while stack:
        node, depth = stack.pop()
        if not isinstance(node, dict):
            _invalid("each node must be a mapping.")
        if id(node) in seen_nodes:
            # Reject reused sub-objects: a snapshot is a tree, not a graph.
            _invalid("duplicate node reference (tree must not share nodes).")
        seen_nodes.add(id(node))

        node_count += 1
        if node_count > MAX_SNAPSHOT_NODES:
            raise SQLParseError(
                f"Snapshot too large (limit {MAX_SNAPSHOT_NODES} nodes)."
            )

        node_type = node.get("type")
        if node_type == "token":
            _path_to_ttype(node.get("ttype"))
            if not isinstance(node.get("value"), str):
                _invalid("token 'value' must be a string.")
            continue

        if node_type == "group":
            group_name = node.get("group")
            if group_name not in _GROUP_REGISTRY:
                raise SQLParseError(f"Unknown group category: {group_name!r}")
            if not isinstance(node.get("value"), str):
                _invalid("group 'value' must be a string.")
            children = node.get("children")
            if not isinstance(children, list) or not children:
                _invalid("group 'children' must be a non-empty list.")
            # ``depth`` is this group's own level (root group = 0).  Its
            # children would occupy depth + 1, so reject before descending:
            # at most MAX_SNAPSHOT_DEPTH group levels (0..limit-1) exist.
            if depth + 1 > MAX_SNAPSHOT_DEPTH:
                raise SQLParseError(
                    f"Snapshot nesting too deep (limit {MAX_SNAPSHOT_DEPTH})."
                )
            for child in children:
                if not isinstance(child, dict):
                    _invalid("group children must be node mappings.")
                stack.append((child, depth + 1))
            continue

        _invalid(f"unknown node type: {node_type!r}")

    return root, node_count


def snapshot_to_tree(snapshot):
    """Restore a token tree from a versioned pure-data snapshot.

    :param snapshot: a dictionary produced by :func:`tree_to_snapshot`
        (or an equivalent decoded structure, e.g. loaded from JSON).
    :returns: the restored :class:`~sqlparse.sql.Token` or
        :class:`~sqlparse.sql.TokenList`, with parent links wired up.
    :raises SQLParseError: if the snapshot is unsupported, malformed, too
        deep or internally inconsistent.
    """
    root, _node_count = _validate_snapshot(snapshot)

    built = {}
    # Post-order iterative construction: children are built before their
    # enclosing group, which also lets us verify value concatenation.
    stack = [(root, False)]
    while stack:
        node, visited = stack.pop()
        node_key = id(node)

        if not visited:
            stack.append((node, True))
            if node["type"] == "group":
                for child in reversed(node["children"]):
                    stack.append((child, False))
            continue

        if node["type"] == "token":
            ttype = _path_to_ttype(node["ttype"])
            built[node_key] = sql.Token(ttype, node["value"])
            continue

        children = [built[id(child)] for child in node["children"]]
        joined = "".join(child.value for child in children)
        if joined != node["value"]:
            raise SQLParseError(
                "Inconsistent snapshot: group 'value' does not match the "
                "concatenation of its subtree values."
            )

        group_cls = _GROUP_REGISTRY[node["group"]]
        group = group_cls(children)
        # TokenList.__init__ derives the value from its children; make sure
        # the stored and reconstructed values really are identical.
        if group.value != node["value"]:
            raise SQLParseError(
                "Inconsistent snapshot: reconstructed group value differs "
                "from the stored value."
            )
        built[node_key] = group

    restored = built[id(root)]
    restored.parent = None
    return restored