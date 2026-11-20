from __future__ import annotations

from collections.abc import Callable
from collections.abc import Iterable
from collections.abc import Mapping
from collections.abc import Sequence
import logging
from typing import Any
from typing import NamedTuple
from typing import TYPE_CHECKING

from ... import util

if TYPE_CHECKING:
    from sqlalchemy.engine import Dialect

log = logging.getLogger(__name__)

TableKey = tuple[str | None, str]

__all__ = ["TableRename", "TableRenames"]


def _format_key(key: TableKey) -> str:
    schema, name = key
    return "%s.%s" % (schema, name) if schema else name


class TableRename(NamedTuple):
    """A single declared table rename, normalized to schema-qualified keys.

    The ``old`` side refers to the table as it currently exists in the
    database; the ``new`` side refers to the table as it is declared in the
    target :class:`~sqlalchemy.schema.MetaData`.

    """

    old_schema: str | None
    old_name: str
    new_schema: str | None
    new_name: str

    @property
    def old_key(self) -> TableKey:
        return (self.old_schema, self.old_name)

    @property
    def new_key(self) -> TableKey:
        return (self.new_schema, self.new_name)

    def __str__(self) -> str:
        return "%s -> %s" % (
            _format_key(self.old_key),
            _format_key(self.new_key),
        )


class TableRenames:
    """The set of explicit table renames declared for an autogenerate run.

    An instance of this class is created from the
    :paramref:`.EnvironmentContext.configure.table_renames` parameter; see
    :ref:`autogenerate_table_renames`.

    """

    def __init__(
        self,
        renames: Sequence[TableRename],
        default_schema: str | None = None,
    ) -> None:
        self.renames = list(renames)
        self.default_schema = default_schema

        # populated by resolve(); until then, no rename is considered to be
        # in effect, so that name translation is a no-op
        self._pairs: list[tuple[TableKey, TableKey]] = []
        self._conn_aliases: dict[TableKey, TableKey] = {}
        self._model_aliases: dict[TableKey, TableKey] = {}

    @classmethod
    def from_option(
        cls,
        value: (
            Mapping[Any, Any]
            | Iterable[tuple[Any, Any]]
            | Sequence[tuple[Any, Any]]
        ),
        dialect: Dialect | None = None,
    ) -> TableRenames:
        """Create a :class:`.TableRenames` from a user-supplied declaration.

        The declaration is a mapping of "old" table names to "new" table
        names; each side may be given either as a plain string, naming a
        table in the default schema, or as a ``(schema, name)`` pair.

        """

        default_schema = getattr(dialect, "default_schema_name", None)

        if isinstance(value, Mapping):
            items: Iterable[tuple[Any, Any]] = list(value.items())
        elif isinstance(value, (str, bytes)):
            raise util.CommandError(
                "table_renames should be a mapping of old to new table "
                "names, got %r" % (value,)
            )
        else:
            items = list(value)

        renames: list[TableRename] = []
        by_source: dict[TableKey, TableRename] = {}
        by_target: dict[TableKey, TableRename] = {}

        for item in items:
            src, tgt = cls._parse_pair(item)

            src_schema, src_name = cls._parse_name(src, "old")
            if isinstance(tgt, str):
                tgt_schema, tgt_name = src_schema, tgt
            else:
                tgt_schema, tgt_name = cls._parse_name(tgt, "new")

            rename = TableRename(
                src_schema,
                src_name,
                tgt_schema,
                tgt_name,
            )
            src_key = (
                cls._normalize_schema(rename.old_schema, default_schema),
                rename.old_name,
            )
            tgt_key = (
                cls._normalize_schema(rename.new_schema, default_schema),
                rename.new_name,
            )

            if src_key == tgt_key:
                raise util.CommandError(
                    "table_renames entry %s renames a table to itself"
                    % (rename,)
                )
            if src_key in by_source:
                raise util.CommandError(
                    "table_renames declares the old table %s more than "
                    "once: %s and %s"
                    % (
                        _format_key(src_key),
                        by_source[src_key],
                        rename,
                    )
                )
            if tgt_key in by_target:
                raise util.CommandError(
                    "table_renames declares the new table %s more than "
                    "once: %s and %s"
                    % (
                        _format_key(tgt_key),
                        by_target[tgt_key],
                        rename,
                    )
                )
            by_source[src_key] = rename
            by_target[tgt_key] = rename
            renames.append(rename)

        for rename in renames:
            src_key = cls._normalize_schema(rename.old_schema, default_schema)
            tgt_key = cls._normalize_schema(rename.new_schema, default_schema)
            if (tgt_key, rename.new_name) in by_source or (
                src_key,
                rename.old_name,
            ) in by_target:
                raise util.CommandError(
                    "table_renames entry %s is chained against another "
                    "entry; each table may only be the old or the new side "
                    "of a single rename" % (rename,)
                )
            if src_key != tgt_key:
                raise util.CommandError(
                    "table_renames entry %s moves a table between schemas; "
                    "a table rename can only change the table name, not "
                    "its schema" % (rename,)
                )

        return cls(renames, default_schema)

    @staticmethod
    def _parse_pair(item: Any) -> tuple[Any, Any]:
        if isinstance(item, (str, bytes)) or not isinstance(
            item, (tuple, list)
        ):
            raise util.CommandError(
                "table_renames entries should be pairs of the form "
                "(old, new); got %r" % (item,)
            )
        if len(item) != 2:
            raise util.CommandError(
                "table_renames entries should be pairs of the form "
                "(old, new); got %r" % (item,)
            )
        return item[0], item[1]

    @staticmethod
    def _parse_name(value: Any, which: str) -> tuple[str | None, str]:
        if isinstance(value, str):
            if not value:
                raise util.CommandError(
                    "table_renames %s table name may not be empty" % which
                )
            return None, value
        elif isinstance(value, (tuple, list)) and len(value) == 2:
            schema, name = value
            if schema is not None and not isinstance(schema, str):
                raise util.CommandError(
                    "table_renames %s schema name should be a string or "
                    "None, got %r" % (which, schema)
                )
            if not isinstance(name, str) or not name:
                raise util.CommandError(
                    "table_renames %s table name should be a non-empty "
                    "string, got %r" % (which, name)
                )
            return schema, name
        else:
            raise util.CommandError(
                "table_renames %s should be given as a table name or a "
                "(schema, name) pair, got %r" % (which, value)
            )

    @staticmethod
    def _normalize_schema(
        schema: str | None, default_schema: str | None = None
    ) -> str | None:
        # tables reported against the default schema are keyed by "None"
        # throughout autogenerate; normalize the declared mapping the same
        # way so that both sides agree
        if schema is not None and schema == default_schema:
            return None
        return schema

    def _norm_key(self, key: TableKey) -> TableKey:
        return (
            self._normalize_schema(key[0], self.default_schema),
            key[1],
        )

    def _check_unqualified(
        self,
        rename: TableRename,
        side: str,
        key: TableKey,
        candidates: Mapping[TableKey, list[TableKey]],
        other: Mapping[TableKey, list[TableKey]],
        other_name: str,
    ) -> None:
        """Report a bare name that could refer to several real tables.

        When one side of a mapping is given without a schema, and that name
        appears in more than one schema while the default-schema table is not
        present, the mapping cannot be matched to a single table.  This is
        only reported when more than one of those tables also has a
        counterpart under ``other_name`` in the same schema, meaning the
        mapping genuinely looks like it could apply to more than one table.

        """

        if key[0] is not None or key in candidates:
            return

        matches = sorted(
            other_key
            for other_key in candidates
            if other_key[1] == key[1]
            and (other_key[0], other_name) in other
        )
        if len(matches) > 1:
            raise util.CommandError(
                "Ambiguous table rename %s; the %s table name %r was given "
                "without a schema and could refer to more than one table "
                "that really exists: %s.  Qualify the mapping with a schema "
                "name"
                % (
                    rename,
                    side,
                    key[1],
                    ", ".join(_format_key(match) for match in matches),
                )
            )

    def resolve(
        self,
        conn_table_names: Iterable[TableKey],
        metadata_table_names: Iterable[TableKey],
        include: Callable[[TableKey], bool] | None = None,
    ) -> list[TableRename]:
        """Determine which declared renames are in effect for this run.

        A rename is in effect when the old table is present in the database
        but no longer in the target metadata, while the new table is present
        in the target metadata but not yet in the database.   Mappings that
        no longer apply, such as those left in place after the rename was
        already applied, are ignored.

        :param include: optional callable, passed each metadata-side key, to
         apply the environment's object filters to the renamed table.

        :class:`.CommandError` is raised when a declaration cannot be
        resolved unambiguously against the tables that really exist; this
        happens before any revision file is generated.

        """

        conn_by_norm: dict[TableKey, list[TableKey]] = {}
        for key in conn_table_names:
            conn_by_norm.setdefault(self._norm_key(key), []).append(key)

        metadata_by_norm: dict[TableKey, list[TableKey]] = {}
        for key in metadata_table_names:
            metadata_by_norm.setdefault(self._norm_key(key), []).append(key)

        applied: list[TableRename] = []
        pairs: list[tuple[TableKey, TableKey]] = []

        for rename in self.renames:
            src = self._norm_key(rename.old_key)
            tgt = self._norm_key(rename.new_key)

            conn_src = conn_by_norm.get(src, [])
            conn_tgt = conn_by_norm.get(tgt, [])
            md_src = metadata_by_norm.get(src, [])
            md_tgt = metadata_by_norm.get(tgt, [])

            if conn_src and conn_tgt:
                raise util.CommandError(
                    "Cannot apply table rename %s; the new table %s already "
                    "exists in the database"
                    % (rename, _format_key(conn_tgt[0]))
                )

            if len(conn_src) > 1 or len(md_tgt) > 1:
                candidates = sorted(
                    _format_key(key)
                    for key in (conn_src if len(conn_src) > 1 else md_tgt)
                )
                side = "database" if len(conn_src) > 1 else "target metadata"
                raise util.CommandError(
                    "Ambiguous table rename %s; the mapping points at more "
                    "than one table that really exists in the %s: %s.  "
                    "Qualify the mapping with a schema name"
                    % (rename, side, ", ".join(candidates))
                )

            if conn_src and md_src and md_tgt:
                raise util.CommandError(
                    "Ambiguous table rename %s; the mapping points at two "
                    "tables that really exist, %s in the database and both "
                    "%s and %s in the target metadata.  Declare the mapping "
                    "with an explicit schema, or remove one of the tables"
                    % (
                        rename,
                        _format_key(conn_src[0]),
                        _format_key(md_src[0]),
                        _format_key(md_tgt[0]),
                    )
                )

            self._check_unqualified(
                rename,
                "old",
                src,
                conn_by_norm,
                metadata_by_norm,
                rename.new_name,
            )
            self._check_unqualified(
                rename,
                "new",
                tgt,
                metadata_by_norm,
                conn_by_norm,
                rename.old_name,
            )

            if not conn_src:
                log.debug(
                    "ignoring table rename %s; the old table %s is not "
                    "present in the database",
                    rename,
                    _format_key(rename.old_key),
                )
                continue

            if not md_tgt:
                log.debug(
                    "ignoring table rename %s; the new table %s is not "
                    "present in the target metadata",
                    rename,
                    _format_key(rename.new_key),
                )
                continue

            if include is not None and not include(md_tgt[0]):
                log.debug(
                    "ignoring table rename %s; table %s is excluded by the "
                    "environment's filters",
                    rename,
                    _format_key(md_tgt[0]),
                )
                continue

            applied.append(rename)
            pairs.append((md_tgt[0], conn_src[0]))

        self._pairs = pairs

        # connection side -> metadata side and vice versa, using the
        # spellings that were actually encountered
        self._conn_aliases = {conn: md for md, conn in pairs}
        self._model_aliases = {md: conn for md, conn in pairs}
        return applied

    @property
    def pairs(self) -> list[tuple[TableKey, TableKey]]:
        """Metadata-side and database-side keys for each applied rename."""

        return list(self._pairs)

    def model_key_for_conn(
        self, schema: str | None, name: str
    ) -> tuple[str | None, str] | None:
        """Return the target metadata key for a database-side table."""

        key = (schema, name)
        if key in self._conn_aliases:
            return self._conn_aliases[key]
        norm = self._norm_key(key)
        for conn_key, md_key in self._conn_aliases.items():
            if self._norm_key(conn_key) == norm:
                return md_key
        return None

    def conn_key_for_model(
        self, schema: str | None, name: str
    ) -> tuple[str | None, str] | None:
        """Return the database-side key for a target metadata table."""

        key = (schema, name)
        if key in self._model_aliases:
            return self._model_aliases[key]
        norm = self._norm_key(key)
        for md_key, conn_key in self._model_aliases.items():
            if self._norm_key(md_key) == norm:
                return conn_key
        return None

    def translate_fk_tokens(
        self, tokens: tuple[Any, ...]
    ) -> tuple[Any, ...]:
        """Translate the table names within a foreign key signature.

        Both the source and the target table names are translated from the
        names present in the database to the names present in the target
        metadata, so that a foreign key that merely follows a renamed table
        is not reported as a change.

        """

        if not self._conn_aliases:
            return tokens

        src_schema, src_table = self.model_key_for_conn(
            tokens[0], tokens[1]
        ) or (tokens[0], tokens[1])
        tgt_schema, tgt_table = self.model_key_for_conn(
            tokens[3], tokens[4]
        ) or (tokens[3], tokens[4])

        return (
            src_schema,
            src_table,
            tokens[2],
            tgt_schema,
            tgt_table,
        ) + tuple(tokens[5:])

    def translate_reflected_fk_sig(self, sig: Any) -> None:
        """Rewrite the table names of a reflected foreign key signature.

        Used so that a foreign key constraint that points at (or is owned by)
        a table being renamed compares equal to its metadata counterpart.

        """

        if not self._conn_aliases:
            return

        sig.translate_table_names(self.translate_fk_tokens)
