from __future__ import annotations

from abc import abstractmethod
from collections.abc import Iterable
from collections.abc import Iterator
from collections.abc import MutableMapping
from collections.abc import Sequence
from dataclasses import dataclass
import enum
import functools
import os
import pathlib
import re
from typing import Any
from typing import Callable
from typing import cast
from typing import TYPE_CHECKING
from typing import TypeVar

from sqlalchemy import types as sqltypes
from sqlalchemy.types import NULLTYPE

from . import schemaobj
from .base import BatchOperations
from .base import Operations
from .. import util
from ..util import sqla_compat

if TYPE_CHECKING:
    from typing import Literal

    from sqlalchemy.sql import Executable
    from sqlalchemy.sql.elements import ColumnElement
    from sqlalchemy.sql.elements import conv
    from sqlalchemy.sql.elements import quoted_name
    from sqlalchemy.sql.elements import TextClause
    from sqlalchemy.sql.schema import CheckConstraint
    from sqlalchemy.sql.schema import Column
    from sqlalchemy.sql.schema import Constraint
    from sqlalchemy.sql.schema import ForeignKeyConstraint
    from sqlalchemy.sql.schema import Index
    from sqlalchemy.sql.schema import MetaData
    from sqlalchemy.sql.schema import PrimaryKeyConstraint
    from sqlalchemy.sql.schema import SchemaItem
    from sqlalchemy.sql.schema import Table
    from sqlalchemy.sql.schema import UniqueConstraint
    from sqlalchemy.sql.selectable import TableClause
    from sqlalchemy.sql.type_api import TypeEngine

    from ..autogenerate.rewriter import Rewriter
    from ..ddl.base import _ServerDefaultArgument
    from ..runtime.migration import MigrationContext
    from ..script.revision import _RevIdType

_T = TypeVar("_T", bound=Any)
_AC = TypeVar("_AC", bound="AddConstraintOp")


class DataImpactType(enum.Enum):
    """Describe the kind of data effect an operation has.

    These values are reported by :func:`.inspect_data_impact`, both for
    built-in operations and for custom operations that declare their own
    impact using :meth:`.MigrateOperation.data_impact`.

    .. versionadded:: 1.20.2

    .. seealso::

        :ref:`data_impact`

    """

    UNKNOWN = "unknown"
    """The operation's data impact is not declared and could not be
    determined.  A conservative reviewer should assume that original
    values may be lost."""

    CREATE_TABLE = "create_table"
    DROP_TABLE = "drop_table"
    RENAME_TABLE = "rename_table"

    ADD_COLUMN = "add_column"
    DROP_COLUMN = "drop_column"
    RENAME_COLUMN = "rename_column"
    ALTER_COLUMN_TYPE = "alter_column_type"
    ALTER_COLUMN_NULLABILITY = "alter_column_nullability"
    ALTER_COLUMN_DEFAULT = "alter_column_server_default"
    ALTER_COLUMN_COMMENT = "alter_column_comment"

    CREATE_INDEX = "create_index"
    DROP_INDEX = "drop_index"
    ADD_CONSTRAINT = "add_constraint"
    DROP_CONSTRAINT = "drop_constraint"

    INSERT_ROWS = "insert_rows"
    TABLE_COMMENT = "table_comment"


_DEFAULT_LOSES_DATA: dict[DataImpactType, bool | None] = {
    impact_type: False for impact_type in DataImpactType
}
_DEFAULT_LOSES_DATA[DataImpactType.DROP_TABLE] = True
_DEFAULT_LOSES_DATA[DataImpactType.DROP_COLUMN] = True
_DEFAULT_LOSES_DATA[DataImpactType.ALTER_COLUMN_TYPE] = None
_DEFAULT_LOSES_DATA[DataImpactType.UNKNOWN] = None


class _Unset:
    """Sentinel marking the ``loses_data`` argument as not provided."""

    def __repr__(self) -> str:
        return "<unset>"


_UNSET = _Unset()


def _resolve_loses_data(
    type_: DataImpactType, loses_data: bool | None | _Unset
) -> bool | None:
    if isinstance(loses_data, _Unset):
        return _DEFAULT_LOSES_DATA[type_]
    else:
        return loses_data


@dataclass(frozen=True)
class DataImpact:
    """A single, read-only statement of what an operation does to stored
    data.

    Instances are produced by :func:`.inspect_data_impact`, or by custom
    operations that return them from
    :meth:`.MigrateOperation.data_impact`.  The
    :attr:`.DataImpact.loses_data` attribute states whether original
    values may be discarded by the change; ``None`` means this could not
    be determined and should be treated conservatively.

    The :meth:`.DataImpact.for_table` and
    :meth:`.DataImpact.for_column` constructors derive a suitable
    ``loses_data`` value from the given :attr:`.DataImpact.type` when one
    is not supplied.

    .. versionadded:: 1.20.2

    .. seealso::

        :ref:`data_impact`

    """

    type: DataImpactType
    table: str | None
    schema: str | None = None
    column: str | None = None
    new_name: str | None = None
    """For rename impacts, the new name of the table or column; indicates
    an identity change where values themselves are retained."""

    loses_data: bool | None = None
    """``True`` if original values may be discarded, ``False`` if they are
    retained or converted reversibly, ``None`` if unknown."""

    detail: str | None = None
    """Optional free-form text, such as the old and new datatypes."""

    @classmethod
    def for_table(
        cls,
        type_: DataImpactType,
        table: str | None,
        *,
        schema: str | None = None,
        new_name: str | None = None,
        loses_data: bool | None | _Unset = _UNSET,
        detail: str | None = None,
    ) -> DataImpact:
        """Convenience constructor for a table-level impact."""
        return cls(
            type_,
            table,
            schema=schema,
            new_name=new_name,
            loses_data=_resolve_loses_data(type_, loses_data),
            detail=detail,
        )

    @classmethod
    def for_column(
        cls,
        type_: DataImpactType,
        table: str | None,
        column: str | None,
        *,
        schema: str | None = None,
        new_name: str | None = None,
        loses_data: bool | None | _Unset = _UNSET,
        detail: str | None = None,
    ) -> DataImpact:
        """Convenience constructor for a column-level impact."""
        return cls(
            type_,
            table,
            schema=schema,
            column=column,
            new_name=new_name,
            loses_data=_resolve_loses_data(type_, loses_data),
            detail=detail,
        )

    @property
    def table_label(self) -> str | None:
        """The table name qualified with its schema, if any."""
        if self.table is None:
            return None
        elif self.schema is None:
            return self.table
        else:
            return f"{self.schema}.{self.table}"

    @property
    def identity_change(self) -> bool:
        """True if this impact renames a table or column, retaining the
        values under a new identity."""
        return self.new_name is not None

    def render(self) -> str:
        """Render this impact as a single human-readable line."""
        location = (
            f"table {self.table_label!r}"
            if self.table_label is not None
            else "unknown table"
        )
        if self.column is not None:
            location += f" column {self.column!r}"

        if self.identity_change:
            location += f" (identity change -> {self.new_name!r})"

        detail = f" [{self.detail}]" if self.detail else ""

        if self.loses_data is True:
            state = "loses original values"
        elif self.loses_data is False:
            state = "preserves original values"
        else:
            state = "data impact unknown"

        return f"[{self.type.value}] {location}{detail} ({state})"

    def __str__(self) -> str:
        return self.render()


@dataclass(frozen=True)
class DataImpactDirection:
    """The data impacts collected for one direction of a migration.

    .. versionadded:: 1.20.2

    .. seealso::

        :ref:`data_impact`

    """

    name: str
    impacts: tuple[DataImpact, ...] = ()
    provided: bool = True
    """False when the inspected object did not include operations for
    this direction, in which case nothing is known about it."""

    @property
    def loses_data(self) -> bool | None:
        """``True`` if any impact in this direction loses original values.

        Returns ``True``, ``False``, or ``None``.  ``False`` means every
        reported impact is known to preserve values; ``None`` means the
        direction was not provided or at least one impact is unknown.
        """
        if not self.provided:
            return None
        outcomes = [impact.loses_data for impact in self.impacts]
        if any(outcome is True for outcome in outcomes):
            return True
        elif any(outcome is None for outcome in outcomes):
            return None
        else:
            return False

    @property
    def may_lose_data(self) -> bool:
        """True unless every impact in this direction is known to preserve
        original values."""
        return self.loses_data is not False

    def by_table(self) -> dict[str, tuple[DataImpact, ...]]:
        """Group impacts by qualified table name, in first-seen order."""
        grouped: dict[str, list[DataImpact]] = {}
        for impact in self.impacts:
            label = impact.table_label or "<unknown>"
            grouped.setdefault(label, []).append(impact)
        return {label: tuple(items) for label, items in grouped.items()}

    def for_table(
        self, table: str, schema: str | None = None
    ) -> list[DataImpact]:
        """Return impacts recorded for a specific table."""
        return [
            impact
            for impact in self.impacts
            if impact.table == table and impact.schema == schema
        ]

    def render(self) -> str:
        """Render this direction as a human-readable block of text."""
        if not self.provided:
            header = f"{self.name}: not available"
        elif not self.impacts:
            header = f"{self.name}: no data impact"
        else:
            if self.loses_data is True:
                header = f"{self.name}: loses original values"
            elif self.loses_data is False:
                header = f"{self.name}: preserves original values"
            else:
                header = f"{self.name}: data impact unknown"

        lines = [header]
        lines.extend(f"  {impact.render()}" for impact in self.impacts)
        return "\n".join(lines)

    def __str__(self) -> str:
        return self.render()


@dataclass(frozen=True)
class DataImpactReport:
    """The result of :func:`.inspect_data_impact`.

    Holds the impacts collected separately for the ``upgrade`` and
    ``downgrade`` directions, so that a reviewer can see at a glance
    which direction discards original values.

    .. versionadded:: 1.20.2

    .. seealso::

        :ref:`data_impact`

    """

    upgrade: DataImpactDirection
    downgrade: DataImpactDirection

    @property
    def directions(self) -> tuple[DataImpactDirection, ...]:
        return (self.upgrade, self.downgrade)

    @property
    def loses_data(self) -> dict[str, bool | None]:
        """Map of direction name to its :attr:`.DataImpactDirection.loses_data`
        value."""
        return {
            direction.name: direction.loses_data
            for direction in self.directions
        }

    def losing_directions(self) -> list[DataImpactDirection]:
        """Return the directions that are known to lose original values."""
        return [
            direction
            for direction in self.directions
            if direction.loses_data is True
        ]

    def render(self) -> str:
        """Render both directions as a human-readable block of text."""
        return "\n".join(direction.render() for direction in self.directions)

    def __str__(self) -> str:
        return self.render()


class MigrateOperation:
    """base class for migration command and organization objects.

    This system is part of the operation extensibility API.

    .. seealso::

        :ref:`operation_objects`

        :ref:`operation_plugins`

        :ref:`customizing_revision`

    """

    @util.memoized_property
    def info(self) -> dict[Any, Any]:
        """A dictionary that may be used to store arbitrary information
        along with this :class:`.MigrateOperation` object.

        """
        return {}

    _mutations: frozenset[Rewriter] = frozenset()

    def reverse(self) -> MigrateOperation:
        raise NotImplementedError

    def to_diff_tuple(self) -> tuple[Any, ...]:
        raise NotImplementedError

    def data_impact(self) -> Iterable[DataImpact] | None:
        """Declare the data impact of this operation.

        The return value is a sequence of :class:`.DataImpact` objects
        describing which tables and columns the operation touches, what
        kind of change it makes, and whether stored values are discarded.
        A caller such as :func:`.inspect_data_impact` uses this to produce
        a read-only, per-direction report of a migration.

        The default implementation returns ``None``, which indicates that
        the impact is not known; the report then lists the operation with
        the :attr:`.DataImpactType.UNKNOWN` category rather than assuming
        that it preserves data.  Returning an empty sequence instead
        declares that the operation has no data impact to report.

        Built-in operations have their impact determined by Alembic
        itself and do not need to override this method.

        .. versionadded:: 1.20.2

        .. seealso::

            :ref:`data_impact`

        """
        return None


class AddConstraintOp(MigrateOperation):
    """Represent an add constraint operation."""

    add_constraint_ops = util.Dispatcher()

    @property
    def constraint_type(self) -> str:
        raise NotImplementedError()

    @classmethod
    def register_add_constraint(
        cls, type_: str
    ) -> Callable[[type[_AC]], type[_AC]]:
        def go(klass: type[_AC]) -> type[_AC]:
            cls.add_constraint_ops.dispatch_for(type_)(klass.from_constraint)
            return klass

        return go

    @classmethod
    def from_constraint(cls, constraint: Constraint) -> AddConstraintOp:
        return cls.add_constraint_ops.dispatch(constraint.__visit_name__)(  # type: ignore[no-any-return]  # noqa: E501
            constraint
        )

    @abstractmethod
    def to_constraint(
        self, migration_context: MigrationContext | None = None
    ) -> Constraint:
        pass

    def reverse(self) -> DropConstraintOp:
        return DropConstraintOp.from_constraint(self.to_constraint())

    def to_diff_tuple(self) -> tuple[str, Constraint]:
        return ("add_constraint", self.to_constraint())


@Operations.register_operation("drop_constraint")
@BatchOperations.register_operation("drop_constraint", "batch_drop_constraint")
class DropConstraintOp(MigrateOperation):
    """Represent a drop constraint operation."""

    def __init__(
        self,
        constraint_name: sqla_compat._ConstraintNameDefined | None,
        table_name: str,
        type_: str | None = None,
        *,
        schema: str | None = None,
        if_exists: bool | None = None,
        _reverse: AddConstraintOp | None = None,
    ) -> None:
        self.constraint_name = constraint_name
        self.table_name = table_name
        self.constraint_type = type_
        self.schema = schema
        self.if_exists = if_exists
        self._reverse = _reverse

    def reverse(self) -> AddConstraintOp:
        return AddConstraintOp.from_constraint(self.to_constraint())

    def to_diff_tuple(
        self,
    ) -> tuple[str, SchemaItem]:
        if self.constraint_type == "foreignkey":
            return ("remove_fk", self.to_constraint())
        else:
            return ("remove_constraint", self.to_constraint())

    @classmethod
    def from_constraint(cls, constraint: Constraint) -> DropConstraintOp:
        types = {
            "unique_constraint": "unique",
            "foreign_key_constraint": "foreignkey",
            "primary_key_constraint": "primary",
            "check_constraint": "check",
            "column_check_constraint": "check",
            "table_or_column_check_constraint": "check",
        }

        constraint_table = sqla_compat._table_for_constraint(constraint)
        return cls(
            sqla_compat.constraint_name_or_none(constraint.name),
            constraint_table.name,
            schema=constraint_table.schema,
            type_=types.get(constraint.__visit_name__),
            _reverse=AddConstraintOp.from_constraint(constraint),
        )

    def to_constraint(self) -> Constraint:
        if self._reverse is not None:
            constraint = self._reverse.to_constraint()
            constraint.name = self.constraint_name
            constraint_table = sqla_compat._table_for_constraint(constraint)
            constraint_table.name = self.table_name
            constraint_table.schema = self.schema

            return constraint
        else:
            raise ValueError(
                "constraint cannot be produced; "
                "original constraint is not present"
            )

    @classmethod
    def drop_constraint(
        cls,
        operations: Operations,
        constraint_name: str,
        table_name: str,
        type_: str | None = None,
        *,
        schema: str | None = None,
        if_exists: bool | None = None,
    ) -> None:
        r"""Drop a constraint of the given name, typically via DROP CONSTRAINT.

        :param constraint_name: name of the constraint.
        :param table_name: table name.
        :param type\_: optional, required on MySQL.  can be
         'foreignkey', 'primary', 'unique', or 'check'.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.
        :param if_exists: If True, adds IF EXISTS operator when
         dropping the constraint

         .. versionadded:: 1.16.0

        """

        op = cls(
            constraint_name,
            table_name,
            type_=type_,
            schema=schema,
            if_exists=if_exists,
        )
        return operations.invoke(op)

    @classmethod
    def batch_drop_constraint(
        cls,
        operations: BatchOperations,
        constraint_name: str,
        type_: str | None = None,
    ) -> None:
        """Issue a "drop constraint" instruction using the
        current batch migration context.

        The batch form of this call omits the ``table_name`` and ``schema``
        arguments from the call.

        .. seealso::

            :meth:`.Operations.drop_constraint`

        """
        op = cls(
            constraint_name,
            operations.impl.table_name,
            type_=type_,
            schema=operations.impl.schema,
        )
        return operations.invoke(op)


@Operations.register_operation("create_primary_key")
@BatchOperations.register_operation(
    "create_primary_key", "batch_create_primary_key"
)
@AddConstraintOp.register_add_constraint("primary_key_constraint")
class CreatePrimaryKeyOp(AddConstraintOp):
    """Represent a create primary key operation."""

    constraint_type = "primarykey"

    def __init__(
        self,
        constraint_name: sqla_compat._ConstraintNameDefined | None,
        table_name: str,
        columns: Sequence[str],
        *,
        schema: str | None = None,
        **kw: Any,
    ) -> None:
        self.constraint_name = constraint_name
        self.table_name = table_name
        self.columns = columns
        self.schema = schema
        self.kw = kw

    @classmethod
    def from_constraint(cls, constraint: Constraint) -> CreatePrimaryKeyOp:
        constraint_table = sqla_compat._table_for_constraint(constraint)
        pk_constraint = cast("PrimaryKeyConstraint", constraint)
        return cls(
            sqla_compat.constraint_name_or_none(pk_constraint.name),
            constraint_table.name,
            pk_constraint.columns.keys(),
            schema=constraint_table.schema,
            **pk_constraint.dialect_kwargs,
        )

    def to_constraint(
        self, migration_context: MigrationContext | None = None
    ) -> PrimaryKeyConstraint:
        schema_obj = schemaobj.SchemaObjects(migration_context)

        return schema_obj.primary_key_constraint(
            self.constraint_name,
            self.table_name,
            self.columns,
            schema=self.schema,
            **self.kw,
        )

    @classmethod
    def create_primary_key(
        cls,
        operations: Operations,
        constraint_name: str | None,
        table_name: str,
        columns: list[str],
        *,
        schema: str | None = None,
    ) -> None:
        """Issue a "create primary key" instruction using the current
        migration context.

        e.g.::

            from alembic import op

            op.create_primary_key("pk_my_table", "my_table", ["id", "version"])

        This internally generates a :class:`~sqlalchemy.schema.Table` object
        containing the necessary columns, then generates a new
        :class:`~sqlalchemy.schema.PrimaryKeyConstraint`
        object which it then associates with the
        :class:`~sqlalchemy.schema.Table`.
        Any event listeners associated with this action will be fired
        off normally.   The :class:`~sqlalchemy.schema.AddConstraint`
        construct is ultimately used to generate the ALTER statement.

        :param constraint_name: Name of the primary key constraint.  The name
         is necessary so that an ALTER statement can be emitted.  For setups
         that use an automated naming scheme such as that described at
         :ref:`sqla:constraint_naming_conventions`
         ``name`` here can be ``None``, as the event listener will
         apply the name to the constraint object when it is associated
         with the table.
        :param table_name: String name of the target table.
        :param columns: a list of string column names to be applied to the
         primary key constraint.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.

        """
        op = cls(constraint_name, table_name, columns, schema=schema)
        return operations.invoke(op)

    @classmethod
    def batch_create_primary_key(
        cls,
        operations: BatchOperations,
        constraint_name: str | None,
        columns: list[str],
    ) -> None:
        """Issue a "create primary key" instruction using the
        current batch migration context.

        The batch form of this call omits the ``table_name`` and ``schema``
        arguments from the call.

        .. seealso::

            :meth:`.Operations.create_primary_key`

        """
        op = cls(
            constraint_name,
            operations.impl.table_name,
            columns,
            schema=operations.impl.schema,
        )
        return operations.invoke(op)


@Operations.register_operation("create_unique_constraint")
@BatchOperations.register_operation(
    "create_unique_constraint", "batch_create_unique_constraint"
)
@AddConstraintOp.register_add_constraint("unique_constraint")
class CreateUniqueConstraintOp(AddConstraintOp):
    """Represent a create unique constraint operation."""

    constraint_type = "unique"

    def __init__(
        self,
        constraint_name: sqla_compat._ConstraintNameDefined | None,
        table_name: str,
        columns: Sequence[str],
        *,
        schema: str | None = None,
        **kw: Any,
    ) -> None:
        self.constraint_name = constraint_name
        self.table_name = table_name
        self.columns = columns
        self.schema = schema
        self.kw = kw

    @classmethod
    def from_constraint(
        cls, constraint: Constraint
    ) -> CreateUniqueConstraintOp:
        constraint_table = sqla_compat._table_for_constraint(constraint)

        uq_constraint = cast("UniqueConstraint", constraint)

        kw: dict[str, Any] = {}
        if uq_constraint.deferrable:
            kw["deferrable"] = uq_constraint.deferrable
        if uq_constraint.initially:
            kw["initially"] = uq_constraint.initially
        kw.update(uq_constraint.dialect_kwargs)
        return cls(
            sqla_compat.constraint_name_or_none(uq_constraint.name),
            constraint_table.name,
            [c.name for c in uq_constraint.columns],
            schema=constraint_table.schema,
            **kw,
        )

    def to_constraint(
        self, migration_context: MigrationContext | None = None
    ) -> UniqueConstraint:
        schema_obj = schemaobj.SchemaObjects(migration_context)
        return schema_obj.unique_constraint(
            self.constraint_name,
            self.table_name,
            self.columns,
            schema=self.schema,
            **self.kw,
        )

    @classmethod
    def create_unique_constraint(
        cls,
        operations: Operations,
        constraint_name: str | None,
        table_name: str,
        columns: Sequence[str],
        *,
        schema: str | None = None,
        **kw: Any,
    ) -> Any:
        """Issue a "create unique constraint" instruction using the
        current migration context.

        e.g.::

            from alembic import op
            op.create_unique_constraint("uq_user_name", "user", ["name"])

        This internally generates a :class:`~sqlalchemy.schema.Table` object
        containing the necessary columns, then generates a new
        :class:`~sqlalchemy.schema.UniqueConstraint`
        object which it then associates with the
        :class:`~sqlalchemy.schema.Table`.
        Any event listeners associated with this action will be fired
        off normally.   The :class:`~sqlalchemy.schema.AddConstraint`
        construct is ultimately used to generate the ALTER statement.

        :param name: Name of the unique constraint.  The name is necessary
         so that an ALTER statement can be emitted.  For setups that
         use an automated naming scheme such as that described at
         :ref:`sqla:constraint_naming_conventions`,
         ``name`` here can be ``None``, as the event listener will
         apply the name to the constraint object when it is associated
         with the table.
        :param table_name: String name of the source table.
        :param columns: a list of string column names in the
         source table.
        :param deferrable: optional bool. If set, emit DEFERRABLE or
         NOT DEFERRABLE when issuing DDL for this constraint.
        :param initially: optional string. If set, emit INITIALLY <value>
         when issuing DDL for this constraint.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.

        """

        op = cls(constraint_name, table_name, columns, schema=schema, **kw)
        return operations.invoke(op)

    @classmethod
    def batch_create_unique_constraint(
        cls,
        operations: BatchOperations,
        constraint_name: str,
        columns: Sequence[str],
        **kw: Any,
    ) -> Any:
        """Issue a "create unique constraint" instruction using the
        current batch migration context.

        The batch form of this call omits the ``source`` and ``schema``
        arguments from the call.

        .. seealso::

            :meth:`.Operations.create_unique_constraint`

        """
        kw["schema"] = operations.impl.schema
        op = cls(constraint_name, operations.impl.table_name, columns, **kw)
        return operations.invoke(op)


@Operations.register_operation("create_foreign_key")
@BatchOperations.register_operation(
    "create_foreign_key", "batch_create_foreign_key"
)
@AddConstraintOp.register_add_constraint("foreign_key_constraint")
class CreateForeignKeyOp(AddConstraintOp):
    """Represent a create foreign key constraint operation."""

    constraint_type = "foreignkey"

    def __init__(
        self,
        constraint_name: sqla_compat._ConstraintNameDefined | None,
        source_table: str,
        referent_table: str,
        local_cols: list[str],
        remote_cols: list[str],
        **kw: Any,
    ) -> None:
        self.constraint_name = constraint_name
        self.source_table = source_table
        self.referent_table = referent_table
        self.local_cols = local_cols
        self.remote_cols = remote_cols
        self.kw = kw

    def to_diff_tuple(self) -> tuple[str, ForeignKeyConstraint]:
        return ("add_fk", self.to_constraint())

    @classmethod
    def from_constraint(cls, constraint: Constraint) -> CreateForeignKeyOp:
        fk_constraint = cast("ForeignKeyConstraint", constraint)
        kw: dict[str, Any] = {}
        if fk_constraint.onupdate:
            kw["onupdate"] = fk_constraint.onupdate
        if fk_constraint.ondelete:
            kw["ondelete"] = fk_constraint.ondelete
        if fk_constraint.initially:
            kw["initially"] = fk_constraint.initially
        if fk_constraint.deferrable:
            kw["deferrable"] = fk_constraint.deferrable
        if fk_constraint.use_alter:
            kw["use_alter"] = fk_constraint.use_alter
        if fk_constraint.match:
            kw["match"] = fk_constraint.match

        (
            source_schema,
            source_table,
            source_columns,
            target_schema,
            target_table,
            target_columns,
            onupdate,
            ondelete,
            deferrable,
            initially,
        ) = sqla_compat._fk_spec(fk_constraint)

        kw["source_schema"] = source_schema
        kw["referent_schema"] = target_schema
        kw.update(fk_constraint.dialect_kwargs)
        return cls(
            sqla_compat.constraint_name_or_none(fk_constraint.name),
            source_table,
            target_table,
            source_columns,
            target_columns,
            **kw,
        )

    def to_constraint(
        self, migration_context: MigrationContext | None = None
    ) -> ForeignKeyConstraint:
        schema_obj = schemaobj.SchemaObjects(migration_context)
        return schema_obj.foreign_key_constraint(
            self.constraint_name,
            self.source_table,
            self.referent_table,
            self.local_cols,
            self.remote_cols,
            **self.kw,
        )

    @classmethod
    def create_foreign_key(
        cls,
        operations: Operations,
        constraint_name: str | None,
        source_table: str,
        referent_table: str,
        local_cols: list[str],
        remote_cols: list[str],
        *,
        onupdate: str | None = None,
        ondelete: str | None = None,
        deferrable: bool | None = None,
        initially: str | None = None,
        match: str | None = None,
        source_schema: str | None = None,
        referent_schema: str | None = None,
        **dialect_kw: Any,
    ) -> None:
        """Issue a "create foreign key" instruction using the
        current migration context.

        e.g.::

            from alembic import op

            op.create_foreign_key(
                "fk_user_address",
                "address",
                "user",
                ["user_id"],
                ["id"],
            )

        This internally generates a :class:`~sqlalchemy.schema.Table` object
        containing the necessary columns, then generates a new
        :class:`~sqlalchemy.schema.ForeignKeyConstraint`
        object which it then associates with the
        :class:`~sqlalchemy.schema.Table`.
        Any event listeners associated with this action will be fired
        off normally.   The :class:`~sqlalchemy.schema.AddConstraint`
        construct is ultimately used to generate the ALTER statement.

        :param constraint_name: Name of the foreign key constraint.  The name
         is necessary so that an ALTER statement can be emitted.  For setups
         that use an automated naming scheme such as that described at
         :ref:`sqla:constraint_naming_conventions`,
         ``name`` here can be ``None``, as the event listener will
         apply the name to the constraint object when it is associated
         with the table.
        :param source_table: String name of the source table.
        :param referent_table: String name of the destination table.
        :param local_cols: a list of string column names in the
         source table.
        :param remote_cols: a list of string column names in the
         remote table.
        :param onupdate: Optional string. If set, emit ON UPDATE <value> when
         issuing DDL for this constraint. Typical values include CASCADE,
         DELETE and RESTRICT.
        :param ondelete: Optional string. If set, emit ON DELETE <value> when
         issuing DDL for this constraint. Typical values include CASCADE,
         DELETE and RESTRICT.
        :param deferrable: optional bool. If set, emit DEFERRABLE or NOT
         DEFERRABLE when issuing DDL for this constraint.
        :param source_schema: Optional schema name of the source table.
        :param referent_schema: Optional schema name of the destination table.

        """

        op = cls(
            constraint_name,
            source_table,
            referent_table,
            local_cols,
            remote_cols,
            onupdate=onupdate,
            ondelete=ondelete,
            deferrable=deferrable,
            source_schema=source_schema,
            referent_schema=referent_schema,
            initially=initially,
            match=match,
            **dialect_kw,
        )
        return operations.invoke(op)

    @classmethod
    def batch_create_foreign_key(
        cls,
        operations: BatchOperations,
        constraint_name: str | None,
        referent_table: str,
        local_cols: list[str],
        remote_cols: list[str],
        *,
        referent_schema: str | None = None,
        onupdate: str | None = None,
        ondelete: str | None = None,
        deferrable: bool | None = None,
        initially: str | None = None,
        match: str | None = None,
        **dialect_kw: Any,
    ) -> None:
        """Issue a "create foreign key" instruction using the
        current batch migration context.

        The batch form of this call omits the ``source`` and ``source_schema``
        arguments from the call.

        e.g.::

            with batch_alter_table("address") as batch_op:
                batch_op.create_foreign_key(
                    "fk_user_address",
                    "user",
                    ["user_id"],
                    ["id"],
                )

        .. seealso::

            :meth:`.Operations.create_foreign_key`

        """
        op = cls(
            constraint_name,
            operations.impl.table_name,
            referent_table,
            local_cols,
            remote_cols,
            onupdate=onupdate,
            ondelete=ondelete,
            deferrable=deferrable,
            source_schema=operations.impl.schema,
            referent_schema=referent_schema,
            initially=initially,
            match=match,
            **dialect_kw,
        )
        return operations.invoke(op)


@Operations.register_operation("create_check_constraint")
@BatchOperations.register_operation(
    "create_check_constraint", "batch_create_check_constraint"
)
@AddConstraintOp.register_add_constraint("check_constraint")
@AddConstraintOp.register_add_constraint("table_or_column_check_constraint")
@AddConstraintOp.register_add_constraint("column_check_constraint")
class CreateCheckConstraintOp(AddConstraintOp):
    """Represent a create check constraint operation."""

    constraint_type = "check"

    def __init__(
        self,
        constraint_name: sqla_compat._ConstraintNameDefined | None,
        table_name: str,
        condition: str | TextClause | ColumnElement[Any],
        *,
        schema: str | None = None,
        **kw: Any,
    ) -> None:
        self.constraint_name = constraint_name
        self.table_name = table_name
        self.condition = condition
        self.schema = schema
        self.kw = kw

    @classmethod
    def from_constraint(
        cls, constraint: Constraint
    ) -> CreateCheckConstraintOp:
        constraint_table = sqla_compat._table_for_constraint(constraint)

        ck_constraint = cast("CheckConstraint", constraint)
        return cls(
            sqla_compat.constraint_name_or_none(ck_constraint.name),
            constraint_table.name,
            cast("ColumnElement[Any]", ck_constraint.sqltext),
            schema=constraint_table.schema,
            **ck_constraint.dialect_kwargs,
        )

    def to_constraint(
        self, migration_context: MigrationContext | None = None
    ) -> CheckConstraint:
        schema_obj = schemaobj.SchemaObjects(migration_context)
        return schema_obj.check_constraint(
            self.constraint_name,
            self.table_name,
            self.condition,
            schema=self.schema,
            **self.kw,
        )

    @classmethod
    def create_check_constraint(
        cls,
        operations: Operations,
        constraint_name: str | None,
        table_name: str,
        condition: str | ColumnElement[bool] | TextClause,
        *,
        schema: str | None = None,
        **kw: Any,
    ) -> None:
        """Issue a "create check constraint" instruction using the
        current migration context.

        e.g.::

            from alembic import op
            from sqlalchemy.sql import column, func

            op.create_check_constraint(
                "ck_user_name_len",
                "user",
                func.len(column("name")) > 5,
            )

        CHECK constraints are usually against a SQL expression, so ad-hoc
        table metadata is usually needed.   The function will convert the given
        arguments into a :class:`sqlalchemy.schema.CheckConstraint` bound
        to an anonymous table in order to emit the CREATE statement.

        :param name: Name of the check constraint.  The name is necessary
         so that an ALTER statement can be emitted.  For setups that
         use an automated naming scheme such as that described at
         :ref:`sqla:constraint_naming_conventions`,
         ``name`` here can be ``None``, as the event listener will
         apply the name to the constraint object when it is associated
         with the table.
        :param table_name: String name of the source table.
        :param condition: SQL expression that's the condition of the
         constraint. Can be a string or SQLAlchemy expression language
         structure.
        :param deferrable: optional bool. If set, emit DEFERRABLE or
         NOT DEFERRABLE when issuing DDL for this constraint.
        :param initially: optional string. If set, emit INITIALLY <value>
         when issuing DDL for this constraint.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.

        """
        op = cls(constraint_name, table_name, condition, schema=schema, **kw)
        return operations.invoke(op)

    @classmethod
    def batch_create_check_constraint(
        cls,
        operations: BatchOperations,
        constraint_name: str,
        condition: str | ColumnElement[bool] | TextClause,
        **kw: Any,
    ) -> None:
        """Issue a "create check constraint" instruction using the
        current batch migration context.

        The batch form of this call omits the ``source`` and ``schema``
        arguments from the call.

        .. seealso::

            :meth:`.Operations.create_check_constraint`

        """
        op = cls(
            constraint_name,
            operations.impl.table_name,
            condition,
            schema=operations.impl.schema,
            **kw,
        )
        return operations.invoke(op)


@Operations.register_operation("create_index")
@BatchOperations.register_operation("create_index", "batch_create_index")
class CreateIndexOp(MigrateOperation):
    """Represent a create index operation."""

    def __init__(
        self,
        index_name: str | None,
        table_name: str,
        columns: Sequence[str | TextClause | ColumnElement[Any]],
        *,
        schema: str | None = None,
        unique: bool = False,
        if_not_exists: bool | None = None,
        **kw: Any,
    ) -> None:
        self.index_name = index_name
        self.table_name = table_name
        self.columns = columns
        self.schema = schema
        self.unique = unique
        self.if_not_exists = if_not_exists
        self.kw = kw

    def reverse(self) -> DropIndexOp:
        return DropIndexOp.from_index(self.to_index())

    def to_diff_tuple(self) -> tuple[str, Index]:
        return ("add_index", self.to_index())

    @classmethod
    def from_index(cls, index: Index) -> CreateIndexOp:
        assert index.table is not None
        return cls(
            index.name,
            index.table.name,
            index.expressions,
            schema=index.table.schema,
            unique=index.unique,
            **index.kwargs,
        )

    def to_index(
        self, migration_context: MigrationContext | None = None
    ) -> Index:
        schema_obj = schemaobj.SchemaObjects(migration_context)

        idx = schema_obj.index(
            self.index_name,
            self.table_name,
            self.columns,
            schema=self.schema,
            unique=self.unique,
            **self.kw,
        )
        return idx

    @classmethod
    def create_index(
        cls,
        operations: Operations,
        index_name: str | None,
        table_name: str,
        columns: Sequence[str | TextClause | ColumnElement[Any]],
        *,
        schema: str | None = None,
        unique: bool = False,
        if_not_exists: bool | None = None,
        **kw: Any,
    ) -> None:
        r"""Issue a "create index" instruction using the current
        migration context.

        e.g.::

            from alembic import op

            op.create_index("ik_test", "t1", ["foo", "bar"])

        Functional indexes can be produced by using the
        :func:`sqlalchemy.sql.expression.text` construct::

            from alembic import op
            from sqlalchemy import text

            op.create_index("ik_test", "t1", [text("lower(foo)")])

        :param index_name: name of the index.
        :param table_name: name of the owning table.
        :param columns: a list consisting of string column names and/or
         :func:`~sqlalchemy.sql.expression.text` constructs.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.
        :param unique: If True, create a unique index.

        :param quote: Force quoting of this column's name on or off,
         corresponding to ``True`` or ``False``. When left at its default
         of ``None``, the column identifier will be quoted according to
         whether the name is case sensitive (identifiers with at least one
         upper case character are treated as case sensitive), or if it's a
         reserved word. This flag is only needed to force quoting of a
         reserved word which is not known by the SQLAlchemy dialect.

        :param if_not_exists: If True, adds IF NOT EXISTS operator when
         creating the new index.

         .. versionadded:: 1.12.0

        :param \**kw: Additional keyword arguments not mentioned above are
         dialect specific, and passed in the form
         ``<dialectname>_<argname>``.
         See the documentation regarding an individual dialect at
         :ref:`dialect_toplevel` for detail on documented arguments.

        """
        op = cls(
            index_name,
            table_name,
            columns,
            schema=schema,
            unique=unique,
            if_not_exists=if_not_exists,
            **kw,
        )
        return operations.invoke(op)

    @classmethod
    def batch_create_index(
        cls,
        operations: BatchOperations,
        index_name: str,
        columns: Sequence[str | TextClause | ColumnElement[Any]],
        **kw: Any,
    ) -> None:
        """Issue a "create index" instruction using the
        current batch migration context.

        .. seealso::

            :meth:`.Operations.create_index`

        """

        op = cls(
            index_name,
            operations.impl.table_name,
            columns,
            schema=operations.impl.schema,
            **kw,
        )
        return operations.invoke(op)


@Operations.register_operation("drop_index")
@BatchOperations.register_operation("drop_index", "batch_drop_index")
class DropIndexOp(MigrateOperation):
    """Represent a drop index operation."""

    def __init__(
        self,
        index_name: quoted_name | str | conv,
        table_name: str | None = None,
        *,
        schema: str | None = None,
        if_exists: bool | None = None,
        _reverse: CreateIndexOp | None = None,
        **kw: Any,
    ) -> None:
        self.index_name = index_name
        self.table_name = table_name
        self.schema = schema
        self.if_exists = if_exists
        self._reverse = _reverse
        self.kw = kw

    def to_diff_tuple(self) -> tuple[str, Index]:
        return ("remove_index", self.to_index())

    def reverse(self) -> CreateIndexOp:
        return CreateIndexOp.from_index(self.to_index())

    @classmethod
    def from_index(cls, index: Index) -> DropIndexOp:
        assert index.table is not None
        return cls(
            index.name,  # type: ignore[arg-type]
            table_name=index.table.name,
            schema=index.table.schema,
            _reverse=CreateIndexOp.from_index(index),
            unique=index.unique,
            **index.kwargs,
        )

    def to_index(
        self, migration_context: MigrationContext | None = None
    ) -> Index:
        schema_obj = schemaobj.SchemaObjects(migration_context)

        # need a dummy column name here since SQLAlchemy
        # 0.7.6 and further raises on Index with no columns
        return schema_obj.index(
            self.index_name,
            self.table_name,
            self._reverse.columns if self._reverse else ["x"],
            schema=self.schema,
            **self.kw,
        )

    @classmethod
    def drop_index(
        cls,
        operations: Operations,
        index_name: str,
        table_name: str | None = None,
        *,
        schema: str | None = None,
        if_exists: bool | None = None,
        **kw: Any,
    ) -> None:
        r"""Issue a "drop index" instruction using the current
        migration context.

        e.g.::

            drop_index("accounts")

        :param index_name: name of the index.
        :param table_name: name of the owning table.  Some
         backends such as Microsoft SQL Server require this.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.

        :param if_exists: If True, adds IF EXISTS operator when
         dropping the index.

         .. versionadded:: 1.12.0

        :param \**kw: Additional keyword arguments not mentioned above are
         dialect specific, and passed in the form
         ``<dialectname>_<argname>``.
         See the documentation regarding an individual dialect at
         :ref:`dialect_toplevel` for detail on documented arguments.

        """
        op = cls(
            index_name,
            table_name=table_name,
            schema=schema,
            if_exists=if_exists,
            **kw,
        )
        return operations.invoke(op)

    @classmethod
    def batch_drop_index(
        cls, operations: BatchOperations, index_name: str, **kw: Any
    ) -> None:
        """Issue a "drop index" instruction using the
        current batch migration context.

        .. seealso::

            :meth:`.Operations.drop_index`

        """

        op = cls(
            index_name,
            table_name=operations.impl.table_name,
            schema=operations.impl.schema,
            **kw,
        )
        return operations.invoke(op)


@Operations.register_operation("create_table")
class CreateTableOp(MigrateOperation):
    """Represent a create table operation."""

    def __init__(
        self,
        table_name: str,
        columns: Sequence[SchemaItem],
        *,
        schema: str | None = None,
        if_not_exists: bool | None = None,
        _namespace_metadata: MetaData | None = None,
        _constraints_included: bool = False,
        **kw: Any,
    ) -> None:
        self.table_name = table_name
        self.columns = columns
        self.schema = schema
        self.if_not_exists = if_not_exists
        self.info = kw.pop("info", {})
        self.comment = kw.pop("comment", None)
        self.prefixes = kw.pop("prefixes", None)
        self.kw = kw
        self._namespace_metadata = _namespace_metadata
        self._constraints_included = _constraints_included

    def reverse(self) -> DropTableOp:
        return DropTableOp.from_table(
            self.to_table(), _namespace_metadata=self._namespace_metadata
        )

    def to_diff_tuple(self) -> tuple[str, Table]:
        return ("add_table", self.to_table())

    @classmethod
    def from_table(
        cls, table: Table, *, _namespace_metadata: MetaData | None = None
    ) -> CreateTableOp:
        if _namespace_metadata is None:
            _namespace_metadata = table.metadata

        return cls(
            table.name,
            list(table.c) + list(table.constraints),
            schema=table.schema,
            _namespace_metadata=_namespace_metadata,
            # given a Table() object, this Table will contain full Index()
            # and UniqueConstraint objects already constructed in response to
            # each unique=True / index=True flag on a Column.  Carry this
            # state along so that when we re-convert back into a Table, we
            # skip unique=True/index=True so that these constraints are
            # not doubled up. see #844 #848
            _constraints_included=True,
            comment=table.comment,
            info=dict(table.info),
            prefixes=list(table._prefixes),
            **table.kwargs,
        )

    def to_table(
        self, migration_context: MigrationContext | None = None
    ) -> Table:
        schema_obj = schemaobj.SchemaObjects(migration_context)

        return schema_obj.table(
            self.table_name,
            *self.columns,
            schema=self.schema,
            prefixes=list(self.prefixes) if self.prefixes else [],
            comment=self.comment,
            info=self.info.copy() if self.info else {},
            _constraints_included=self._constraints_included,
            **self.kw,
        )

    @classmethod
    def create_table(
        cls,
        operations: Operations,
        table_name: str,
        *columns: SchemaItem,
        if_not_exists: bool | None = None,
        **kw: Any,
    ) -> Table:
        r"""Issue a "create table" instruction using the current migration
        context.

        This directive receives an argument list similar to that of the
        traditional :class:`sqlalchemy.schema.Table` construct, but without the
        metadata::

            from sqlalchemy import INTEGER, VARCHAR, NVARCHAR, Column
            from alembic import op

            op.create_table(
                "account",
                Column("id", INTEGER, primary_key=True),
                Column("name", VARCHAR(50), nullable=False),
                Column("description", NVARCHAR(200)),
                Column("timestamp", TIMESTAMP, server_default=func.now()),
            )

        Note that :meth:`.create_table` accepts
        :class:`~sqlalchemy.schema.Column`
        constructs directly from the SQLAlchemy library.  In particular,
        default values to be created on the database side are
        specified using the ``server_default`` parameter, and not
        ``default`` which only specifies Python-side defaults::

            from alembic import op
            from sqlalchemy import Column, TIMESTAMP, func

            # specify "DEFAULT NOW" along with the "timestamp" column
            op.create_table(
                "account",
                Column("id", INTEGER, primary_key=True),
                Column("timestamp", TIMESTAMP, server_default=func.now()),
            )

        The function also returns a newly created
        :class:`~sqlalchemy.schema.Table` object, corresponding to the table
        specification given, which is suitable for
        immediate SQL operations, in particular
        :meth:`.Operations.bulk_insert`::

            from sqlalchemy import INTEGER, VARCHAR, NVARCHAR, Column
            from alembic import op

            account_table = op.create_table(
                "account",
                Column("id", INTEGER, primary_key=True),
                Column("name", VARCHAR(50), nullable=False),
                Column("description", NVARCHAR(200)),
                Column("timestamp", TIMESTAMP, server_default=func.now()),
            )

            op.bulk_insert(
                account_table,
                [
                    {"name": "A1", "description": "account 1"},
                    {"name": "A2", "description": "account 2"},
                ],
            )

        :param table_name: Name of the table
        :param \*columns: collection of :class:`~sqlalchemy.schema.Column`
         objects within
         the table, as well as optional :class:`~sqlalchemy.schema.Constraint`
         objects
         and :class:`~.sqlalchemy.schema.Index` objects.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.
        :param if_not_exists: If True, adds IF NOT EXISTS operator when
         creating the new table.

         .. versionadded:: 1.13.3
        :param \**kw: Other keyword arguments are passed to the underlying
         :class:`sqlalchemy.schema.Table` object created for the command.

        :return: the :class:`~sqlalchemy.schema.Table` object corresponding
         to the parameters given.

        """
        op = cls(table_name, columns, if_not_exists=if_not_exists, **kw)
        return operations.invoke(op)


@Operations.register_operation("drop_table")
class DropTableOp(MigrateOperation):
    """Represent a drop table operation."""

    def __init__(
        self,
        table_name: str,
        *,
        schema: str | None = None,
        if_exists: bool | None = None,
        table_kw: MutableMapping[Any, Any] | None = None,
        _reverse: CreateTableOp | None = None,
    ) -> None:
        self.table_name = table_name
        self.schema = schema
        self.if_exists = if_exists
        self.table_kw = table_kw or {}
        self.comment = self.table_kw.pop("comment", None)
        self.info = self.table_kw.pop("info", None)
        self.prefixes = self.table_kw.pop("prefixes", None)
        self._reverse = _reverse

    def to_diff_tuple(self) -> tuple[str, Table]:
        return ("remove_table", self.to_table())

    def reverse(self) -> CreateTableOp:
        return CreateTableOp.from_table(self.to_table())

    @classmethod
    def from_table(
        cls, table: Table, *, _namespace_metadata: MetaData | None = None
    ) -> DropTableOp:
        return cls(
            table.name,
            schema=table.schema,
            table_kw={
                "comment": table.comment,
                "info": dict(table.info),
                "prefixes": list(table._prefixes),
                **table.kwargs,
            },
            _reverse=CreateTableOp.from_table(
                table, _namespace_metadata=_namespace_metadata
            ),
        )

    def to_table(
        self, migration_context: MigrationContext | None = None
    ) -> Table:
        if self._reverse:
            cols_and_constraints = self._reverse.columns
        else:
            cols_and_constraints = []

        schema_obj = schemaobj.SchemaObjects(migration_context)
        t = schema_obj.table(
            self.table_name,
            *cols_and_constraints,
            comment=self.comment,
            info=self.info.copy() if self.info else {},
            prefixes=list(self.prefixes) if self.prefixes else [],
            schema=self.schema,
            _constraints_included=(
                self._reverse._constraints_included if self._reverse else False
            ),
            **self.table_kw,
        )
        return t

    @classmethod
    def drop_table(
        cls,
        operations: Operations,
        table_name: str,
        *,
        schema: str | None = None,
        if_exists: bool | None = None,
        **kw: Any,
    ) -> None:
        r"""Issue a "drop table" instruction using the current
        migration context.


        e.g.::

            drop_table("accounts")

        :param table_name: Name of the table
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.
        :param if_exists: If True, adds IF EXISTS operator when
         dropping the table.

         .. versionadded:: 1.13.3
        :param \**kw: Other keyword arguments are passed to the underlying
         :class:`sqlalchemy.schema.Table` object created for the command.

        """
        op = cls(table_name, schema=schema, if_exists=if_exists, table_kw=kw)
        operations.invoke(op)


class AlterTableOp(MigrateOperation):
    """Represent an alter table operation."""

    def __init__(
        self,
        table_name: str,
        *,
        schema: str | None = None,
    ) -> None:
        self.table_name = table_name
        self.schema = schema


@Operations.register_operation("rename_table")
class RenameTableOp(AlterTableOp):
    """Represent a rename table operation."""

    def __init__(
        self,
        old_table_name: str,
        new_table_name: str,
        *,
        schema: str | None = None,
    ) -> None:
        super().__init__(old_table_name, schema=schema)
        self.new_table_name = new_table_name

    @classmethod
    def rename_table(
        cls,
        operations: Operations,
        old_table_name: str,
        new_table_name: str,
        *,
        schema: str | None = None,
    ) -> None:
        """Emit an ALTER TABLE to rename a table.

        :param old_table_name: old name.
        :param new_table_name: new name.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.

        """
        op = cls(old_table_name, new_table_name, schema=schema)
        return operations.invoke(op)


@Operations.register_operation("create_table_comment")
@BatchOperations.register_operation(
    "create_table_comment", "batch_create_table_comment"
)
class CreateTableCommentOp(AlterTableOp):
    """Represent a COMMENT ON `table` operation."""

    def __init__(
        self,
        table_name: str,
        comment: str | None,
        *,
        schema: str | None = None,
        existing_comment: str | None = None,
    ) -> None:
        self.table_name = table_name
        self.comment = comment
        self.existing_comment = existing_comment
        self.schema = schema

    @classmethod
    def create_table_comment(
        cls,
        operations: Operations,
        table_name: str,
        comment: str | None,
        *,
        existing_comment: str | None = None,
        schema: str | None = None,
    ) -> None:
        """Emit a COMMENT ON operation to set the comment for a table.

        :param table_name: string name of the target table.
        :param comment: string value of the comment being registered against
         the specified table.
        :param existing_comment: String value of a comment
         already registered on the specified table, used within autogenerate
         so that the operation is reversible, but not required for direct
         use.

        .. seealso::

            :meth:`.Operations.drop_table_comment`

            :paramref:`.Operations.alter_column.comment`

        """

        op = cls(
            table_name,
            comment,
            existing_comment=existing_comment,
            schema=schema,
        )
        return operations.invoke(op)

    @classmethod
    def batch_create_table_comment(
        cls,
        operations: BatchOperations,
        comment: str | None,
        *,
        existing_comment: str | None = None,
    ) -> None:
        """Emit a COMMENT ON operation to set the comment for a table
        using the current batch migration context.

        :param comment: string value of the comment being registered against
         the specified table.
        :param existing_comment: String value of a comment
         already registered on the specified table, used within autogenerate
         so that the operation is reversible, but not required for direct
         use.

        """

        op = cls(
            operations.impl.table_name,
            comment,
            existing_comment=existing_comment,
            schema=operations.impl.schema,
        )
        return operations.invoke(op)

    def reverse(self) -> CreateTableCommentOp | DropTableCommentOp:
        """Reverses the COMMENT ON operation against a table."""
        if self.existing_comment is None:
            return DropTableCommentOp(
                self.table_name,
                existing_comment=self.comment,
                schema=self.schema,
            )
        else:
            return CreateTableCommentOp(
                self.table_name,
                self.existing_comment,
                existing_comment=self.comment,
                schema=self.schema,
            )

    def to_table(
        self, migration_context: MigrationContext | None = None
    ) -> Table:
        schema_obj = schemaobj.SchemaObjects(migration_context)

        return schema_obj.table(
            self.table_name, schema=self.schema, comment=self.comment
        )

    def to_diff_tuple(self) -> tuple[Any, ...]:
        return ("add_table_comment", self.to_table(), self.existing_comment)


@Operations.register_operation("drop_table_comment")
@BatchOperations.register_operation(
    "drop_table_comment", "batch_drop_table_comment"
)
class DropTableCommentOp(AlterTableOp):
    """Represent an operation to remove the comment from a table."""

    def __init__(
        self,
        table_name: str,
        *,
        schema: str | None = None,
        existing_comment: str | None = None,
    ) -> None:
        self.table_name = table_name
        self.existing_comment = existing_comment
        self.schema = schema

    @classmethod
    def drop_table_comment(
        cls,
        operations: Operations,
        table_name: str,
        *,
        existing_comment: str | None = None,
        schema: str | None = None,
    ) -> None:
        """Issue a "drop table comment" operation to
        remove an existing comment set on a table.

        :param table_name: string name of the target table.
        :param existing_comment: An optional string value of a comment already
         registered on the specified table.

        .. seealso::

            :meth:`.Operations.create_table_comment`

            :paramref:`.Operations.alter_column.comment`

        """

        op = cls(table_name, existing_comment=existing_comment, schema=schema)
        return operations.invoke(op)

    @classmethod
    def batch_drop_table_comment(
        cls,
        operations: BatchOperations,
        *,
        existing_comment: str | None = None,
    ) -> None:
        """Issue a "drop table comment" operation to
        remove an existing comment set on a table using the current
        batch operations context.

        :param existing_comment: An optional string value of a comment already
         registered on the specified table.

        """

        op = cls(
            operations.impl.table_name,
            existing_comment=existing_comment,
            schema=operations.impl.schema,
        )
        return operations.invoke(op)

    def reverse(self) -> CreateTableCommentOp:
        """Reverses the COMMENT ON operation against a table."""
        return CreateTableCommentOp(
            self.table_name, self.existing_comment, schema=self.schema
        )

    def to_table(
        self, migration_context: MigrationContext | None = None
    ) -> Table:
        schema_obj = schemaobj.SchemaObjects(migration_context)

        return schema_obj.table(self.table_name, schema=self.schema)

    def to_diff_tuple(self) -> tuple[Any, ...]:
        return ("remove_table_comment", self.to_table())


@Operations.register_operation("alter_column")
@BatchOperations.register_operation("alter_column", "batch_alter_column")
class AlterColumnOp(AlterTableOp):
    """Represent an alter column operation."""

    def __init__(
        self,
        table_name: str,
        column_name: str,
        *,
        schema: str | None = None,
        existing_type: Any | None = None,
        existing_server_default: _ServerDefaultArgument = False,
        existing_nullable: bool | None = None,
        existing_comment: str | None = None,
        modify_nullable: bool | None = None,
        modify_comment: str | Literal[False] | None = False,
        modify_server_default: Any = False,
        modify_name: str | None = None,
        modify_type: Any | None = None,
        **kw: Any,
    ) -> None:
        super().__init__(table_name, schema=schema)
        self.column_name = column_name
        self.existing_type = existing_type
        self.existing_server_default = existing_server_default
        self.existing_nullable = existing_nullable
        self.existing_comment = existing_comment
        self.modify_nullable = modify_nullable
        self.modify_comment = modify_comment
        self.modify_server_default = modify_server_default
        self.modify_name = modify_name
        self.modify_type = modify_type
        self.kw = kw

    def to_diff_tuple(self) -> Any:
        col_diff = []
        schema, tname, cname = self.schema, self.table_name, self.column_name

        if self.modify_type is not None:
            col_diff.append(
                (
                    "modify_type",
                    schema,
                    tname,
                    cname,
                    {
                        "existing_nullable": self.existing_nullable,
                        "existing_server_default": (
                            self.existing_server_default
                        ),
                        "existing_comment": self.existing_comment,
                    },
                    self.existing_type,
                    self.modify_type,
                )
            )

        if self.modify_nullable is not None:
            col_diff.append(
                (
                    "modify_nullable",
                    schema,
                    tname,
                    cname,
                    {
                        "existing_type": self.existing_type,
                        "existing_server_default": (
                            self.existing_server_default
                        ),
                        "existing_comment": self.existing_comment,
                    },
                    self.existing_nullable,
                    self.modify_nullable,
                )
            )

        if self.modify_server_default is not False:
            col_diff.append(
                (
                    "modify_default",
                    schema,
                    tname,
                    cname,
                    {
                        "existing_nullable": self.existing_nullable,
                        "existing_type": self.existing_type,
                        "existing_comment": self.existing_comment,
                    },
                    self.existing_server_default,
                    self.modify_server_default,
                )
            )

        if self.modify_comment is not False:
            col_diff.append(
                (
                    "modify_comment",
                    schema,
                    tname,
                    cname,
                    {
                        "existing_nullable": self.existing_nullable,
                        "existing_type": self.existing_type,
                        "existing_server_default": (
                            self.existing_server_default
                        ),
                    },
                    self.existing_comment,
                    self.modify_comment,
                )
            )

        return col_diff

    def has_changes(self) -> bool:
        hc1 = (
            self.modify_nullable is not None
            or self.modify_server_default is not False
            or self.modify_type is not None
            or self.modify_comment is not False
        )
        if hc1:
            return True
        for kw in self.kw:
            if kw.startswith("modify_"):
                return True
        else:
            return False

    def reverse(self) -> AlterColumnOp:
        kw = self.kw.copy()
        kw["existing_type"] = self.existing_type
        kw["existing_nullable"] = self.existing_nullable
        kw["existing_server_default"] = self.existing_server_default
        kw["existing_comment"] = self.existing_comment
        if self.modify_type is not None:
            kw["modify_type"] = self.modify_type
        if self.modify_nullable is not None:
            kw["modify_nullable"] = self.modify_nullable
        if self.modify_server_default is not False:
            kw["modify_server_default"] = self.modify_server_default
        if self.modify_comment is not False:
            kw["modify_comment"] = self.modify_comment

        # TODO: make this a little simpler
        all_keys = {
            m.group(1)
            for m in [re.match(r"^(?:existing_|modify_)(.+)$", k) for k in kw]
            if m
        }

        for k in all_keys:
            if "modify_%s" % k in kw:
                swap = kw["existing_%s" % k]
                kw["existing_%s" % k] = kw["modify_%s" % k]
                kw["modify_%s" % k] = swap

        return self.__class__(
            self.table_name, self.column_name, schema=self.schema, **kw
        )

    @classmethod
    def alter_column(
        cls,
        operations: Operations,
        table_name: str,
        column_name: str,
        *,
        nullable: bool | None = None,
        comment: str | Literal[False] | None = False,
        server_default: _ServerDefaultArgument = False,
        new_column_name: str | None = None,
        type_: TypeEngine[Any] | type[TypeEngine[Any]] | None = None,
        existing_type: None | TypeEngine[Any] | type[TypeEngine[Any]] = None,
        existing_server_default: _ServerDefaultArgument = False,
        existing_nullable: bool | None = None,
        existing_comment: str | None = None,
        schema: str | None = None,
        **kw: Any,
    ) -> None:
        r"""Issue an "alter column" instruction using the
        current migration context.

        Generally, only that aspect of the column which
        is being changed, i.e. name, type, nullability,
        default, needs to be specified.  Multiple changes
        can also be specified at once and the backend should
        "do the right thing", emitting each change either
        separately or together as the backend allows.

        MySQL has special requirements here, since MySQL
        cannot ALTER a column without a full specification.
        When producing MySQL-compatible migration files,
        it is recommended that the ``existing_type``,
        ``existing_server_default``, and ``existing_nullable``
        parameters be present, if not being altered.

        Type changes which are against the SQLAlchemy
        "schema" types :class:`~sqlalchemy.types.Boolean`
        and  :class:`~sqlalchemy.types.Enum` may also
        add or drop constraints which accompany those
        types on backends that don't support them natively.
        The ``existing_type`` argument is
        used in this case to identify and remove a previous
        constraint that was bound to the type object.

        :param table_name: string name of the target table.
        :param column_name: string name of the target column,
         as it exists before the operation begins.
        :param nullable: Optional; specify ``True`` or ``False``
         to alter the column's nullability.
        :param server_default: Optional; specify a string
         SQL expression, :func:`~sqlalchemy.sql.expression.text`,
         or :class:`~sqlalchemy.schema.DefaultClause` to indicate
         an alteration to the column's default value.
         Set to ``None`` to have the default removed.
        :param comment: optional string text of a new comment to add to the
         column.
        :param new_column_name: Optional; specify a string name here to
         indicate the new name within a column rename operation.
        :param type\_: Optional; a :class:`~sqlalchemy.types.TypeEngine`
         type object to specify a change to the column's type.
         For SQLAlchemy types that also indicate a constraint (i.e.
         :class:`~sqlalchemy.types.Boolean`, :class:`~sqlalchemy.types.Enum`),
         the constraint is also generated.
        :param autoincrement: set the ``AUTO_INCREMENT`` flag of the column;
         currently understood by the MySQL dialect.
        :param existing_type: Optional; a
         :class:`~sqlalchemy.types.TypeEngine`
         type object to specify the previous type.   This
         is required for all MySQL column alter operations that
         don't otherwise specify a new type, as well as for
         when nullability is being changed on a SQL Server
         column.  It is also used if the type is a so-called
         SQLAlchemy "schema" type which may define a constraint (i.e.
         :class:`~sqlalchemy.types.Boolean`,
         :class:`~sqlalchemy.types.Enum`),
         so that the constraint can be dropped.
        :param existing_server_default: Optional; The existing
         default value of the column.   Required on MySQL if
         an existing default is not being changed; else MySQL
         removes the default.
        :param existing_nullable: Optional; the existing nullability
         of the column.  Required on MySQL if the existing nullability
         is not being changed; else MySQL sets this to NULL.
        :param existing_autoincrement: Optional; the existing autoincrement
         of the column.  Used for MySQL's system of altering a column
         that specifies ``AUTO_INCREMENT``.
        :param existing_comment: string text of the existing comment on the
         column to be maintained.  Required on MySQL if the existing comment
         on the column is not being changed.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.
        :param postgresql_using: String argument which will indicate a
         SQL expression to render within the Postgresql-specific USING clause
         within ALTER COLUMN.    This string is taken directly as raw SQL which
         must explicitly include any necessary quoting or escaping of tokens
         within the expression.

        """

        alt = cls(
            table_name,
            column_name,
            schema=schema,
            existing_type=existing_type,
            existing_server_default=existing_server_default,
            existing_nullable=existing_nullable,
            existing_comment=existing_comment,
            modify_name=new_column_name,
            modify_type=type_,
            modify_server_default=server_default,
            modify_nullable=nullable,
            modify_comment=comment,
            **kw,
        )

        return operations.invoke(alt)

    @classmethod
    def batch_alter_column(
        cls,
        operations: BatchOperations,
        column_name: str,
        *,
        nullable: bool | None = None,
        comment: str | Literal[False] | None = False,
        server_default: _ServerDefaultArgument = False,
        new_column_name: str | None = None,
        type_: TypeEngine[Any] | type[TypeEngine[Any]] | None = None,
        existing_type: None | TypeEngine[Any] | type[TypeEngine[Any]] = None,
        existing_server_default: _ServerDefaultArgument = False,
        existing_nullable: bool | None = None,
        existing_comment: str | None = None,
        insert_before: str | None = None,
        insert_after: str | None = None,
        **kw: Any,
    ) -> None:
        """Issue an "alter column" instruction using the current
        batch migration context.

        Parameters are the same as that of :meth:`.Operations.alter_column`,
        as well as the following option(s):

        :param insert_before: String name of an existing column which this
         column should be placed before, when creating the new table.

        :param insert_after: String name of an existing column which this
         column should be placed after, when creating the new table.  If
         both :paramref:`.BatchOperations.alter_column.insert_before`
         and :paramref:`.BatchOperations.alter_column.insert_after` are
         omitted, the column is inserted after the last existing column
         in the table.

        .. seealso::

            :meth:`.Operations.alter_column`


        """
        alt = cls(
            operations.impl.table_name,
            column_name,
            schema=operations.impl.schema,
            existing_type=existing_type,
            existing_server_default=existing_server_default,
            existing_nullable=existing_nullable,
            existing_comment=existing_comment,
            modify_name=new_column_name,
            modify_type=type_,
            modify_server_default=server_default,
            modify_nullable=nullable,
            modify_comment=comment,
            insert_before=insert_before,
            insert_after=insert_after,
            **kw,
        )

        return operations.invoke(alt)


@Operations.register_operation("add_column")
@BatchOperations.register_operation("add_column", "batch_add_column")
class AddColumnOp(AlterTableOp):
    """Represent an add column operation."""

    def __init__(
        self,
        table_name: str,
        column: Column[Any],
        *,
        schema: str | None = None,
        if_not_exists: bool | None = None,
        inline_references: bool | None = None,
        inline_primary_key: bool | None = None,
        **kw: Any,
    ) -> None:
        super().__init__(table_name, schema=schema)
        self.column = column
        self.if_not_exists = if_not_exists
        self.inline_references = inline_references
        self.inline_primary_key = inline_primary_key
        self.kw = kw

    def reverse(self) -> DropColumnOp:
        op = DropColumnOp.from_column_and_tablename(
            self.schema, self.table_name, self.column
        )
        op.if_exists = self.if_not_exists
        return op

    def to_diff_tuple(
        self,
    ) -> tuple[str, str | None, str, Column[Any]]:
        return ("add_column", self.schema, self.table_name, self.column)

    def to_column(self) -> Column[Any]:
        return self.column

    @classmethod
    def from_column(cls, col: Column[Any]) -> AddColumnOp:
        return cls(col.table.name, col, schema=col.table.schema)

    @classmethod
    def from_column_and_tablename(
        cls,
        schema: str | None,
        tname: str,
        col: Column[Any],
    ) -> AddColumnOp:
        return cls(tname, col, schema=schema)

    @classmethod
    def add_column(
        cls,
        operations: Operations,
        table_name: str,
        column: Column[Any],
        *,
        schema: str | None = None,
        if_not_exists: bool | None = None,
        inline_references: bool | None = None,
        inline_primary_key: bool | None = None,
    ) -> None:
        """Issue an "add column" instruction using the current
        migration context.

        e.g.::

            from alembic import op
            from sqlalchemy import Column, String

            op.add_column("organization", Column("name", String()))

        The :meth:`.Operations.add_column` method typically corresponds
        to the SQL command "ALTER TABLE... ADD COLUMN".    Within the scope
        of this command, the column's name, datatype, nullability,
        and optional server-generated defaults may be indicated.  Options
        also exist for control of single-column primary key and foreign key
        constraints to be generated.

        .. note::

            Not all constraint types may be indicated with this directive.
            NOT NULL, FOREIGN KEY, and CHECK are honored, PRIMARY KEY
            is conditionally honored, UNIQUE
            is currently not.

            As of 1.18.2, the following :class:`~sqlalchemy.schema.Column`
            parameters are **ignored**:

            * :paramref:`~sqlalchemy.schema.Column.unique` - use the
              :meth:`.Operations.create_unique_constraint` method
            * :paramref:`~sqlalchemy.schema.Column.index` - use the
              :meth:`.Operations.create_index` method

        **PRIMARY KEY support**

        The provided :class:`~sqlalchemy.schema.Column` object may include a
        ``primary_key=True`` directive, indicating the column intends to be
        part of a primary key constraint.  However by default, the inline
        "PRIMARY KEY" directive is not emitted, and it's assumed that a
        separate :meth:`.Operations.create_primary_key` directive will be used
        to create this constraint, which may potentially include other columns
        as well as have an explicit name.   To instead render an inline
        "PRIMARY KEY" directive, the
        :paramref:`.AddColumnOp.inline_primary_key` parameter may be indicated
        at the same time as the ``primary_key`` parameter (both are needed)::

            from alembic import op
            from sqlalchemy import Column, INTEGER

            op.add_column(
                "organization",
                Column("id", INTEGER, primary_key=True),
                inline_primary_key=True
            )

        The ``primary_key=True`` parameter on
        :class:`~sqlalchemy.schema.Column` also indicates behaviors such as
        using the ``SERIAL`` datatype with the PostgreSQL database, which is
        why two separate, independent parameters are provided to support all
        combinations.

        .. versionadded:: 1.18.4 Added
           :paramref:`.AddColumnOp.inline_primary_key`
           to control use of the ``PRIMARY KEY`` inline directive.

        **FOREIGN KEY support**

        The provided :class:`~sqlalchemy.schema.Column` object may include a
        :class:`~sqlalchemy.schema.ForeignKey` constraint directive,
        referencing a remote table name. By default, Alembic will automatically
        emit a second ALTER statement in order to add the single-column FOREIGN
        KEY constraint separately::

            from alembic import op
            from sqlalchemy import Column, INTEGER, ForeignKey

            op.add_column(
                "organization",
                Column("account_id", INTEGER, ForeignKey("accounts.id")),
            )

        To render the FOREIGN KEY constraint inline within the ADD COLUMN
        directive, use the ``inline_references`` parameter. This can improve
        performance on large tables since the constraint is marked as valid
        immediately for nullable columns::

            from alembic import op
            from sqlalchemy import Column, INTEGER, ForeignKey

            op.add_column(
                "organization",
                Column("account_id", INTEGER, ForeignKey("accounts.id")),
                inline_references=True,
            )

        **Indicating server side defaults**

        The column argument passed to :meth:`.Operations.add_column` is a
        :class:`~sqlalchemy.schema.Column` construct, used in the same way it's
        used in SQLAlchemy. In particular, values or functions to be indicated
        as producing the column's default value on the database side are
        specified using the ``server_default`` parameter, and not ``default``
        which only specifies Python-side defaults::

            from alembic import op
            from sqlalchemy import Column, TIMESTAMP, func

            # specify "DEFAULT NOW" along with the column add
            op.add_column(
                "account",
                Column("timestamp", TIMESTAMP, server_default=func.now()),
            )

        :param table_name: String name of the parent table.
        :param column: a :class:`sqlalchemy.schema.Column` object
         representing the new column.
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.
        :param if_not_exists: If True, adds ``IF NOT EXISTS`` operator
         when creating the new column for compatible dialects

         .. versionadded:: 1.16.0

        :param inline_references: If True, renders ``FOREIGN KEY`` constraints
         inline within the ``ADD COLUMN`` directive using ``REFERENCES``
         syntax, rather than as a separate ``ALTER TABLE ADD CONSTRAINT``
         statement. This is supported by PostgreSQL, Oracle, MySQL 5.7+, and
         MariaDB 10.5+.

         .. versionadded:: 1.18.2

        :param inline_primary_key: If True, renders the ``PRIMARY KEY`` phrase
         inline within the ``ADD COLUMN`` directive.  When not present or
         False, ``PRIMARY KEY`` is not emitted; it is assumed that the
         migration script will include an additional
         :meth:`.Operations.create_primary_key` directive to create a full
         primary key constraint.

         .. versionadded:: 1.18.4

        """

        op = cls(
            table_name,
            column,
            schema=schema,
            if_not_exists=if_not_exists,
            inline_references=inline_references,
            inline_primary_key=inline_primary_key,
        )
        return operations.invoke(op)

    @classmethod
    def batch_add_column(
        cls,
        operations: BatchOperations,
        column: Column[Any],
        *,
        insert_before: str | None = None,
        insert_after: str | None = None,
        if_not_exists: bool | None = None,
        inline_references: bool | None = None,
        inline_primary_key: bool | None = None,
    ) -> None:
        """Issue an "add column" instruction using the current
        batch migration context.

        .. seealso::

            :meth:`.Operations.add_column`

        """

        kw = {}
        if insert_before:
            kw["insert_before"] = insert_before
        if insert_after:
            kw["insert_after"] = insert_after

        op = cls(
            operations.impl.table_name,
            column,
            schema=operations.impl.schema,
            if_not_exists=if_not_exists,
            inline_references=inline_references,
            inline_primary_key=inline_primary_key,
            **kw,
        )
        return operations.invoke(op)


@Operations.register_operation("drop_column")
@BatchOperations.register_operation("drop_column", "batch_drop_column")
class DropColumnOp(AlterTableOp):
    """Represent a drop column operation."""

    def __init__(
        self,
        table_name: str,
        column_name: str,
        *,
        schema: str | None = None,
        if_exists: bool | None = None,
        _reverse: AddColumnOp | None = None,
        **kw: Any,
    ) -> None:
        super().__init__(table_name, schema=schema)
        self.column_name = column_name
        self.kw = kw
        self.if_exists = if_exists
        self._reverse = _reverse

    def to_diff_tuple(
        self,
    ) -> tuple[str, str | None, str, Column[Any]]:
        return (
            "remove_column",
            self.schema,
            self.table_name,
            self.to_column(),
        )

    def reverse(self) -> AddColumnOp:
        if self._reverse is None:
            raise ValueError(
                "operation is not reversible; "
                "original column is not present"
            )

        op = AddColumnOp.from_column_and_tablename(
            self.schema, self.table_name, self._reverse.column
        )
        op.if_not_exists = self.if_exists
        return op

    @classmethod
    def from_column_and_tablename(
        cls,
        schema: str | None,
        tname: str,
        col: Column[Any],
    ) -> DropColumnOp:
        return cls(
            tname,
            col.name,
            schema=schema,
            _reverse=AddColumnOp.from_column_and_tablename(schema, tname, col),
        )

    def to_column(
        self, migration_context: MigrationContext | None = None
    ) -> Column[Any]:
        if self._reverse is not None:
            return self._reverse.column
        schema_obj = schemaobj.SchemaObjects(migration_context)
        return schema_obj.column(self.column_name, NULLTYPE)

    @classmethod
    def drop_column(
        cls,
        operations: Operations,
        table_name: str,
        column_name: str,
        *,
        schema: str | None = None,
        **kw: Any,
    ) -> None:
        """Issue a "drop column" instruction using the current
        migration context.

        e.g.::

            drop_column("organization", "account_id")

        :param table_name: name of table
        :param column_name: name of column
        :param schema: Optional schema name to operate within.  To control
         quoting of the schema outside of the default behavior, use
         the SQLAlchemy construct
         :class:`~sqlalchemy.sql.elements.quoted_name`.
        :param if_exists: If True, adds IF EXISTS operator when
         dropping the new column for compatible dialects

         .. versionadded:: 1.16.0

        :param mssql_drop_check: Optional boolean.  When ``True``, on
         Microsoft SQL Server only, first
         drop the CHECK constraint on the column using a
         SQL-script-compatible
         block that selects into a @variable from sys.check_constraints,
         then exec's a separate DROP CONSTRAINT for that constraint.
        :param mssql_drop_default: Optional boolean.  When ``True``, on
         Microsoft SQL Server only, first
         drop the DEFAULT constraint on the column using a
         SQL-script-compatible
         block that selects into a @variable from sys.default_constraints,
         then exec's a separate DROP CONSTRAINT for that default.
        :param mssql_drop_foreign_key: Optional boolean.  When ``True``, on
         Microsoft SQL Server only, first
         drop a single FOREIGN KEY constraint on the column using a
         SQL-script-compatible
         block that selects into a @variable from
         sys.foreign_keys/sys.foreign_key_columns,
         then exec's a separate DROP CONSTRAINT for that default.  Only
         works if the column has exactly one FK constraint which refers to
         it, at the moment.
        """

        op = cls(table_name, column_name, schema=schema, **kw)
        return operations.invoke(op)

    @classmethod
    def batch_drop_column(
        cls, operations: BatchOperations, column_name: str, **kw: Any
    ) -> None:
        """Issue a "drop column" instruction using the current
        batch migration context.

        .. seealso::

            :meth:`.Operations.drop_column`

        """
        op = cls(
            operations.impl.table_name,
            column_name,
            schema=operations.impl.schema,
            **kw,
        )
        return operations.invoke(op)


@Operations.register_operation("bulk_insert")
class BulkInsertOp(MigrateOperation):
    """Represent a bulk insert operation."""

    def __init__(
        self,
        table: Table | TableClause,
        rows: list[dict[str, Any]],
        *,
        multiinsert: bool = True,
    ) -> None:
        self.table = table
        self.rows = rows
        self.multiinsert = multiinsert

    @classmethod
    def bulk_insert(
        cls,
        operations: Operations,
        table: Table | TableClause,
        rows: list[dict[str, Any]],
        *,
        multiinsert: bool = True,
    ) -> None:
        """Issue a "bulk insert" operation using the current
        migration context.

        This provides a means of representing an INSERT of multiple rows
        which works equally well in the context of executing on a live
        connection as well as that of generating a SQL script.   In the
        case of a SQL script, the values are rendered inline into the
        statement.

        e.g.::

            from alembic import op
            from datetime import date
            from sqlalchemy.sql import table, column
            from sqlalchemy import String, Integer, Date

            # Create an ad-hoc table to use for the insert statement.
            accounts_table = table(
                "account",
                column("id", Integer),
                column("name", String),
                column("create_date", Date),
            )

            op.bulk_insert(
                accounts_table,
                [
                    {
                        "id": 1,
                        "name": "John Smith",
                        "create_date": date(2010, 10, 5),
                    },
                    {
                        "id": 2,
                        "name": "Ed Williams",
                        "create_date": date(2007, 5, 27),
                    },
                    {
                        "id": 3,
                        "name": "Wendy Jones",
                        "create_date": date(2008, 8, 15),
                    },
                ],
            )

        When using --sql mode, some datatypes may not render inline
        automatically, such as dates and other special types.   When this
        issue is present, :meth:`.Operations.inline_literal` may be used::

            op.bulk_insert(
                accounts_table,
                [
                    {
                        "id": 1,
                        "name": "John Smith",
                        "create_date": op.inline_literal("2010-10-05"),
                    },
                    {
                        "id": 2,
                        "name": "Ed Williams",
                        "create_date": op.inline_literal("2007-05-27"),
                    },
                    {
                        "id": 3,
                        "name": "Wendy Jones",
                        "create_date": op.inline_literal("2008-08-15"),
                    },
                ],
                multiinsert=False,
            )

        When using :meth:`.Operations.inline_literal` in conjunction with
        :meth:`.Operations.bulk_insert`, in order for the statement to work
        in "online" (e.g. non --sql) mode, the
        :paramref:`~.Operations.bulk_insert.multiinsert`
        flag should be set to ``False``, which will have the effect of
        individual INSERT statements being emitted to the database, each
        with a distinct VALUES clause, so that the "inline" values can
        still be rendered, rather than attempting to pass the values
        as bound parameters.

        :param table: a table object which represents the target of the INSERT.

        :param rows: a list of dictionaries indicating rows.

        :param multiinsert: when at its default of True and --sql mode is not
           enabled, the INSERT statement will be executed using
           "executemany()" style, where all elements in the list of
           dictionaries are passed as bound parameters in a single
           list.   Setting this to False results in individual INSERT
           statements being emitted per parameter set, and is needed
           in those cases where non-literal values are present in the
           parameter sets.

        """

        op = cls(table, rows, multiinsert=multiinsert)
        operations.invoke(op)


@Operations.register_operation("execute")
@BatchOperations.register_operation("execute", "batch_execute")
class ExecuteSQLOp(MigrateOperation):
    """Represent an execute SQL operation."""

    def __init__(
        self,
        sqltext: Executable | str,
        *,
        execution_options: dict[str, Any] | None = None,
    ) -> None:
        self.sqltext = sqltext
        self.execution_options = execution_options

    @classmethod
    def execute(
        cls,
        operations: Operations,
        sqltext: Executable | str,
        *,
        execution_options: dict[str, Any] | None = None,
    ) -> None:
        r"""Execute the given SQL using the current migration context.

        The given SQL can be a plain string, e.g.::

            op.execute("INSERT INTO table (foo) VALUES ('some value')")

        Or it can be any kind of Core SQL Expression construct, such as
        below where we use an update construct::

            from sqlalchemy.sql import table, column
            from sqlalchemy import String
            from alembic import op

            account = table("account", column("name", String))
            op.execute(
                account.update()
                .where(account.c.name == op.inline_literal("account 1"))
                .values({"name": op.inline_literal("account 2")})
            )

        Above, we made use of the SQLAlchemy
        :func:`sqlalchemy.sql.expression.table` and
        :func:`sqlalchemy.sql.expression.column` constructs to make a brief,
        ad-hoc table construct just for our UPDATE statement.  A full
        :class:`~sqlalchemy.schema.Table` construct of course works perfectly
        fine as well, though note it's a recommended practice to at least
        ensure the definition of a table is self-contained within the migration
        script, rather than imported from a module that may break compatibility
        with older migrations.

        In a SQL script context, the statement is emitted directly to the
        output stream.   There is *no* return result, however, as this
        function is oriented towards generating a change script
        that can run in "offline" mode.     Additionally, parameterized
        statements are discouraged here, as they *will not work* in offline
        mode.  Above, we use :meth:`.inline_literal` where parameters are
        to be used.

        For full interaction with a connected database where parameters can
        also be used normally, use the "bind" available from the context::

            from alembic import op

            connection = op.get_bind()

            connection.execute(
                account.update()
                .where(account.c.name == "account 1")
                .values({"name": "account 2"})
            )

        Additionally, when passing the statement as a plain string, it is first
        coerced into a :func:`sqlalchemy.sql.expression.text` construct
        before being passed along.  In the less likely case that the
        literal SQL string contains a colon, it must be escaped with a
        backslash, as::

           op.execute(r"INSERT INTO table (foo) VALUES ('\:colon_value')")


        :param sqltext: Any legal SQLAlchemy expression, including:

        * a string
        * a :func:`sqlalchemy.sql.expression.text` construct.
        * a :func:`sqlalchemy.sql.expression.insert` construct.
        * a :func:`sqlalchemy.sql.expression.update` construct.
        * a :func:`sqlalchemy.sql.expression.delete` construct.
        * Any "executable" described in SQLAlchemy Core documentation,
          noting that no result set is returned.

        .. note::  when passing a plain string, the statement is coerced into
           a :func:`sqlalchemy.sql.expression.text` construct. This construct
           considers symbols with colons, e.g. ``:foo`` to be bound parameters.
           To avoid this, ensure that colon symbols are escaped, e.g.
           ``\:foo``.

        :param execution_options: Optional dictionary of
         execution options, will be passed to
         :meth:`sqlalchemy.engine.Connection.execution_options`.
        """
        op = cls(sqltext, execution_options=execution_options)
        return operations.invoke(op)

    @classmethod
    def batch_execute(
        cls,
        operations: Operations,
        sqltext: Executable | str,
        *,
        execution_options: dict[str, Any] | None = None,
    ) -> None:
        """Execute the given SQL using the current migration context.

        .. seealso::

            :meth:`.Operations.execute`

        """
        return cls.execute(
            operations, sqltext, execution_options=execution_options
        )

    def to_diff_tuple(self) -> tuple[str, Executable | str]:
        return ("execute", self.sqltext)


class OpContainer(MigrateOperation):
    """Represent a sequence of operations operation."""

    def __init__(self, ops: Sequence[MigrateOperation] = ()) -> None:
        self.ops = list(ops)

    def is_empty(self) -> bool:
        return not self.ops

    def as_diffs(self) -> Any:
        return list(OpContainer._ops_as_diffs(self))

    @classmethod
    def _ops_as_diffs(
        cls, migrations: OpContainer
    ) -> Iterator[tuple[Any, ...]]:
        for op in migrations.ops:
            if hasattr(op, "ops"):
                yield from cls._ops_as_diffs(cast("OpContainer", op))
            else:
                yield op.to_diff_tuple()


class ModifyTableOps(OpContainer):
    """Contains a sequence of operations that all apply to a single Table."""

    def __init__(
        self,
        table_name: str,
        ops: Sequence[MigrateOperation],
        *,
        schema: str | None = None,
    ) -> None:
        super().__init__(ops)
        self.table_name = table_name
        self.schema = schema

    def reverse(self) -> ModifyTableOps:
        return ModifyTableOps(
            self.table_name,
            ops=list(reversed([op.reverse() for op in self.ops])),
            schema=self.schema,
        )


class UpgradeOps(OpContainer):
    """contains a sequence of operations that would apply to the
    'upgrade' stream of a script.

    .. seealso::

        :ref:`customizing_revision`

    """

    def __init__(
        self,
        ops: Sequence[MigrateOperation] = (),
        upgrade_token: str = "upgrades",
    ) -> None:
        super().__init__(ops=ops)
        self.upgrade_token = upgrade_token

    def reverse_into(self, downgrade_ops: DowngradeOps) -> DowngradeOps:
        downgrade_ops.ops[:] = list(
            reversed([op.reverse() for op in self.ops])
        )
        return downgrade_ops

    def reverse(self) -> DowngradeOps:
        return self.reverse_into(DowngradeOps(ops=[]))


class DowngradeOps(OpContainer):
    """contains a sequence of operations that would apply to the
    'downgrade' stream of a script.

    .. seealso::

        :ref:`customizing_revision`

    """

    def __init__(
        self,
        ops: Sequence[MigrateOperation] = (),
        downgrade_token: str = "downgrades",
    ) -> None:
        super().__init__(ops=ops)
        self.downgrade_token = downgrade_token

    def reverse(self) -> UpgradeOps:
        return UpgradeOps(
            ops=list(reversed([op.reverse() for op in self.ops]))
        )


class MigrationScript(MigrateOperation):
    """represents a migration script.

    E.g. when autogenerate encounters this object, this corresponds to the
    production of an actual script file.

    A normal :class:`.MigrationScript` object would contain a single
    :class:`.UpgradeOps` and a single :class:`.DowngradeOps` directive.
    These are accessible via the ``.upgrade_ops`` and ``.downgrade_ops``
    attributes.

    In the case of an autogenerate operation that runs multiple times,
    such as the multiple database example in the "multidb" template,
    the ``.upgrade_ops`` and ``.downgrade_ops`` attributes are disabled,
    and instead these objects should be accessed via the ``.upgrade_ops_list``
    and ``.downgrade_ops_list`` list-based attributes.  These latter
    attributes are always available at the very least as single-element lists.

    .. seealso::

        :ref:`customizing_revision`

    """

    _needs_render: bool | None
    _upgrade_ops: list[UpgradeOps]
    _downgrade_ops: list[DowngradeOps]

    def __init__(
        self,
        rev_id: str | None,
        upgrade_ops: UpgradeOps,
        downgrade_ops: DowngradeOps,
        *,
        message: str | None = None,
        imports: set[str] = set(),
        head: str | None = None,
        splice: bool | None = None,
        branch_label: _RevIdType | None = None,
        version_path: str | os.PathLike[str] | None = None,
        depends_on: _RevIdType | None = None,
    ) -> None:
        self.rev_id = rev_id
        self.message = message
        self.imports = imports
        self.head = head
        self.splice = splice
        self.branch_label = branch_label
        self.version_path = (
            pathlib.Path(version_path).as_posix() if version_path else None
        )
        self.depends_on = depends_on
        self.upgrade_ops = upgrade_ops
        self.downgrade_ops = downgrade_ops

    @property
    def upgrade_ops(self) -> UpgradeOps | None:
        """An instance of :class:`.UpgradeOps`.

        .. seealso::

            :attr:`.MigrationScript.upgrade_ops_list`
        """
        if len(self._upgrade_ops) > 1:
            raise ValueError(
                "This MigrationScript instance has a multiple-entry "
                "list for UpgradeOps; please use the "
                "upgrade_ops_list attribute."
            )
        elif not self._upgrade_ops:
            return None
        else:
            return self._upgrade_ops[0]

    @upgrade_ops.setter
    def upgrade_ops(self, upgrade_ops: UpgradeOps | list[UpgradeOps]) -> None:
        self._upgrade_ops = util.to_list(upgrade_ops)
        for elem in self._upgrade_ops:
            assert isinstance(elem, UpgradeOps)

    @property
    def downgrade_ops(self) -> DowngradeOps | None:
        """An instance of :class:`.DowngradeOps`.

        .. seealso::

            :attr:`.MigrationScript.downgrade_ops_list`
        """
        if len(self._downgrade_ops) > 1:
            raise ValueError(
                "This MigrationScript instance has a multiple-entry "
                "list for DowngradeOps; please use the "
                "downgrade_ops_list attribute."
            )
        elif not self._downgrade_ops:
            return None
        else:
            return self._downgrade_ops[0]

    @downgrade_ops.setter
    def downgrade_ops(
        self, downgrade_ops: DowngradeOps | list[DowngradeOps]
    ) -> None:
        self._downgrade_ops = util.to_list(downgrade_ops)
        for elem in self._downgrade_ops:
            assert isinstance(elem, DowngradeOps)

    @property
    def upgrade_ops_list(self) -> list[UpgradeOps]:
        """A list of :class:`.UpgradeOps` instances.

        This is used in place of the :attr:`.MigrationScript.upgrade_ops`
        attribute when dealing with a revision operation that does
        multiple autogenerate passes.

        """
        return self._upgrade_ops

    @property
    def downgrade_ops_list(self) -> list[DowngradeOps]:
        """A list of :class:`.DowngradeOps` instances.

        This is used in place of the :attr:`.MigrationScript.downgrade_ops`
        attribute when dealing with a revision operation that does
        multiple autogenerate passes.

        """
        return self._downgrade_ops

    def inspect_data_impact(self) -> DataImpactReport:
        """Return a read-only data impact report for this migration.

        This is a convenience shorthand for
        ``inspect_data_impact(self)``.

        .. versionadded:: 1.20.2

        .. seealso::

            :func:`.inspect_data_impact`

        """
        return inspect_data_impact(self)


def _type_instance(type_: Any) -> Any:
    if type_ is None or isinstance(type_, sqltypes.TypeEngine):
        return type_
    try:
        return sqltypes.to_instance(type_)
    except Exception:
        return None


def _type_detail(
    existing_type: Any, new_type: Any
) -> tuple[str | None, bool | None]:
    """Return a ``(detail, loses_data)`` pair for a type modification.

    ``loses_data`` is ``False`` when the change is regarded as
    reversible with respect to stored values, ``True`` when the new type
    is narrower than the old one, and ``None`` when there is not enough
    information to say.

    """
    existing = _type_instance(existing_type)
    new = _type_instance(new_type)

    if existing is None or new is None:
        detail = None
        if existing_type is not None or new_type is not None:
            detail = f"{existing_type!r} -> {new_type!r}"
        return detail, None

    detail = f"{existing} -> {new}"

    existing_affinity = existing._type_affinity
    new_affinity = new._type_affinity
    if existing_affinity is not new_affinity:
        # a change of affinity may or may not round-trip; a reviewer
        # should look at the two types rather than be told it is safe
        return detail, None

    existing_length = getattr(existing, "length", None)
    new_length = getattr(new, "length", None)
    if existing_length is not None and new_length is not None:
        return detail, new_length < existing_length
    elif existing_length is None and new_length is not None:
        # an unbounded type is being made bounded; values may be truncated
        return detail, True
    elif existing_length is not None and new_length is None:
        return detail, False

    existing_precision = getattr(existing, "precision", None)
    new_precision = getattr(new, "precision", None)
    if existing_precision is not None and new_precision is not None:
        if new_precision < existing_precision:
            return detail, True
        elif new_precision > existing_precision:
            return detail, False
        else:
            existing_scale = getattr(existing, "scale", None)
            new_scale = getattr(new, "scale", None)
            if existing_scale is not None and new_scale is not None:
                return detail, new_scale < existing_scale
            else:
                return detail, False

    if type(existing) is type(new):
        return detail, False

    # same affinity but a different type class and no size information
    # to compare, e.g. Integer vs. BigInteger
    return detail, None


def _alter_column_impacts(op: AlterColumnOp) -> list[DataImpact]:
    impacts = []

    if op.modify_name is not None and op.modify_name != op.column_name:
        impacts.append(
            DataImpact.for_column(
                DataImpactType.RENAME_COLUMN,
                op.table_name,
                op.column_name,
                schema=op.schema,
                new_name=op.modify_name,
            )
        )

    if op.modify_type is not None:
        detail, loses_data = _type_detail(op.existing_type, op.modify_type)
        impacts.append(
            DataImpact.for_column(
                DataImpactType.ALTER_COLUMN_TYPE,
                op.table_name,
                op.column_name,
                schema=op.schema,
                loses_data=loses_data,
                detail=detail,
            )
        )

    if op.modify_nullable is not None:
        impacts.append(
            DataImpact.for_column(
                DataImpactType.ALTER_COLUMN_NULLABILITY,
                op.table_name,
                op.column_name,
                schema=op.schema,
                detail=(
                    "nullability -> %s"
                    % ("NULL" if op.modify_nullable else "NOT NULL")
                ),
            )
        )

    if op.modify_server_default is not False:
        impacts.append(
            DataImpact.for_column(
                DataImpactType.ALTER_COLUMN_DEFAULT,
                op.table_name,
                op.column_name,
                schema=op.schema,
            )
        )

    if op.modify_comment is not False:
        impacts.append(
            DataImpact.for_column(
                DataImpactType.ALTER_COLUMN_COMMENT,
                op.table_name,
                op.column_name,
                schema=op.schema,
            )
        )

    return impacts


def _create_table_impacts(op: CreateTableOp) -> list[DataImpact]:
    return [
        DataImpact.for_table(
            DataImpactType.CREATE_TABLE, op.table_name, schema=op.schema
        )
    ]


def _drop_table_impacts(op: DropTableOp) -> list[DataImpact]:
    column_names = [
        col.name
        for col in (op._reverse.columns if op._reverse is not None else ())
        if getattr(col, "name", None) is not None
    ]
    detail = (
        "columns: %s" % ", ".join(column_names) if column_names else None
    )
    return [
        DataImpact.for_table(
            DataImpactType.DROP_TABLE,
            op.table_name,
            schema=op.schema,
            detail=detail,
        )
    ]


def _rename_table_impacts(op: RenameTableOp) -> list[DataImpact]:
    return [
        DataImpact.for_table(
            DataImpactType.RENAME_TABLE,
            op.table_name,
            schema=op.schema,
            new_name=op.new_table_name,
        )
    ]


def _add_column_impacts(op: AddColumnOp) -> list[DataImpact]:
    return [
        DataImpact.for_column(
            DataImpactType.ADD_COLUMN,
            op.table_name,
            op.column.name,
            schema=op.schema,
        )
    ]


def _drop_column_impacts(op: DropColumnOp) -> list[DataImpact]:
    return [
        DataImpact.for_column(
            DataImpactType.DROP_COLUMN,
            op.table_name,
            op.column_name,
            schema=op.schema,
        )
    ]


def _add_constraint_impacts(
    op: AddConstraintOp | DropConstraintOp,
    type_: DataImpactType,
) -> list[DataImpact]:
    table_name = getattr(op, "table_name", None)
    if table_name is None:
        table_name = getattr(op, "source_table", None)
    schema = getattr(op, "schema", None)
    if schema is None:
        schema = getattr(op, "source_schema", None)

    constraint_type = getattr(op, "constraint_type", None)

    if getattr(op, "local_cols", None):
        columns = [
            str(col)
            for col in op.local_cols  # type: ignore[attr-defined]
        ]
    else:
        columns = [
            str(col)
            for col in getattr(op, "columns", ())
            if col is not None
        ]

    detail_parts = []
    if constraint_type:
        detail_parts.append(str(constraint_type))
    if columns:
        detail_parts.append("columns: %s" % ", ".join(dict.fromkeys(columns)))

    return [
        DataImpact.for_table(
            type_,
            table_name,
            schema=schema,
            detail="; ".join(detail_parts) or None,
        )
    ]


def _create_index_impacts(op: CreateIndexOp) -> list[DataImpact]:
    return [
        DataImpact.for_table(
            DataImpactType.CREATE_INDEX,
            op.table_name,
            schema=op.schema,
            detail="index: %s" % op.index_name if op.index_name else None,
        )
    ]


def _drop_index_impacts(op: DropIndexOp) -> list[DataImpact]:
    return [
        DataImpact.for_table(
            DataImpactType.DROP_INDEX,
            op.table_name,
            schema=op.schema,
            detail="index: %s" % op.index_name if op.index_name else None,
        )
    ]


def _table_comment_impacts(
    op: CreateTableCommentOp | DropTableCommentOp,
) -> list[DataImpact]:
    return [
        DataImpact.for_table(
            DataImpactType.TABLE_COMMENT, op.table_name, schema=op.schema
        )
    ]


def _bulk_insert_impacts(op: BulkInsertOp) -> list[DataImpact]:
    return [
        DataImpact.for_table(
            DataImpactType.INSERT_ROWS,
            getattr(op.table, "name", None),
            schema=getattr(op.table, "schema", None),
            detail="%d row(s)" % len(op.rows),
        )
    ]


def _execute_sql_impacts(op: ExecuteSQLOp) -> list[DataImpact]:
    sqltext = op.sqltext
    if not isinstance(sqltext, str):
        sqltext = str(sqltext)
    detail = " ".join(sqltext.split())
    if len(detail) > 60:
        detail = detail[:57] + "..."
    return [
        DataImpact(
            DataImpactType.UNKNOWN,
            None,
            detail=detail or None,
        )
    ]


_impact_handlers: dict[type[Any], Callable[..., list[DataImpact]]] = {
    CreateTableOp: _create_table_impacts,
    DropTableOp: _drop_table_impacts,
    RenameTableOp: _rename_table_impacts,
    AddColumnOp: _add_column_impacts,
    DropColumnOp: _drop_column_impacts,
    AlterColumnOp: _alter_column_impacts,
    CreateIndexOp: _create_index_impacts,
    DropIndexOp: _drop_index_impacts,
    CreateTableCommentOp: _table_comment_impacts,
    DropTableCommentOp: _table_comment_impacts,
    BulkInsertOp: _bulk_insert_impacts,
    ExecuteSQLOp: _execute_sql_impacts,
}


def _builtin_impact_handler(
    op: MigrateOperation,
) -> Callable[..., list[DataImpact]] | None:
    for cls in type(op).__mro__:
        handler = _impact_handlers.get(cls)
        if handler is not None:
            return handler
    if isinstance(op, AddConstraintOp):
        return functools.partial(
            _add_constraint_impacts, type_=DataImpactType.ADD_CONSTRAINT
        )
    elif isinstance(op, DropConstraintOp):
        return functools.partial(
            _add_constraint_impacts, type_=DataImpactType.DROP_CONSTRAINT
        )
    else:
        return None


def _declares_data_impact(op: MigrateOperation) -> bool:
    return type(op).data_impact is not MigrateOperation.data_impact


def _apply_defaults(
    impacts: list[DataImpact],
    default_table: str | None,
    default_schema: str | None,
) -> list[DataImpact]:
    """Fill in table/schema on impacts where the operation did not
    provide them, using the enclosing batch container's values."""
    if default_table is None and default_schema is None:
        return impacts

    patched: list[DataImpact] = []
    for impact in impacts:
        table = impact.table if impact.table is not None else default_table
        schema = (
            impact.schema if impact.schema is not None else default_schema
        )
        if table is impact.table and schema is impact.schema:
            patched.append(impact)
        else:
            patched.append(
                DataImpact(
                    impact.type,
                    table,
                    schema=schema,
                    column=impact.column,
                    new_name=impact.new_name,
                    loses_data=impact.loses_data,
                    detail=impact.detail,
                )
            )
    return patched


def _operation_impacts(
    op: MigrateOperation,
    default_table: str | None = None,
    default_schema: str | None = None,
) -> list[DataImpact]:
    handler = _builtin_impact_handler(op)
    if handler is not None:
        return _apply_defaults(handler(op), default_table, default_schema)

    table = getattr(op, "table_name", None) or default_table
    schema = getattr(op, "schema", None) or default_schema
    column = getattr(op, "column_name", None)
    if column is None:
        column_obj = getattr(op, "column", None)
        column = getattr(column_obj, "name", None)

    return [
        DataImpact(
            DataImpactType.UNKNOWN,
            table,
            schema=schema,
            column=column,
            detail=type(op).__name__,
        )
    ]


def _collect_impacts(
    op: MigrateOperation,
    default_table: str | None = None,
    default_schema: str | None = None,
) -> list[DataImpact]:
    if _declares_data_impact(op):
        declared = op.data_impact()
        if declared is not None:
            return list(declared)

    if isinstance(op, ModifyTableOps):
        table = op.table_name if op.table_name is not None else default_table
        schema = op.schema if op.schema is not None else default_schema
        impacts: list[DataImpact] = []
        for child in op.ops:
            impacts.extend(_collect_impacts(child, table, schema))
        return impacts
    elif isinstance(op, OpContainer):
        impacts = []
        for child in op.ops:
            impacts.extend(
                _collect_impacts(child, default_table, default_schema)
            )
        return impacts
    else:
        return _operation_impacts(op, default_table, default_schema)


def _direction_from_containers(
    name: str,
    containers: Sequence[OpContainer],
) -> DataImpactDirection:
    impacts: list[DataImpact] = []
    for container in containers:
        impacts.extend(_collect_impacts(container))
    return DataImpactDirection(
        name, tuple(impacts), provided=bool(containers)
    )


def inspect_data_impact(subject: Any) -> DataImpactReport:
    """Produce a read-only report of the data impact of a migration.

    Given a :class:`.MigrationScript`, an operation container such as
    :class:`.UpgradeOps`, :class:`.DowngradeOps` or
    :class:`.ModifyTableOps`, or a single :class:`.MigrateOperation`,
    return a :class:`.DataImpactReport` that lists, separately for the
    ``upgrade`` and ``downgrade`` directions, the tables, columns and
    :class:`.DataImpactType` categories affected.

    The inspection does not modify the operation tree, invoke any
    operation, or render SQL.  The same objects may afterwards be passed
    to :func:`.render_python_code`, run through an
    :class:`.Operations` instance, or otherwise used exactly as before;
    the report is purely descriptive.

    Operations nested inside batch / table containers such as
    :class:`.ModifyTableOps` are expanded so that each individual
    directive is reported, rather than the container as a whole.  A
    rename is reported with :attr:`.DataImpact.new_name` set, indicating
    an identity change in which stored values are retained.

    For operations whose impact Alembic does not know, including custom
    operations that do not declare one, the category
    :attr:`.DataImpactType.UNKNOWN` is used and
    :attr:`.DataImpact.loses_data` is ``None``, which a reviewer should
    treat conservatively as possible data loss.  Custom operations may
    declare their own impact by overriding
    :meth:`.MigrateOperation.data_impact`.

    :param subject: a :class:`.MigrationScript`, an
     :class:`.OpContainer`, or a single :class:`.MigrateOperation`.
    :return: a :class:`.DataImpactReport`.

    .. versionadded:: 1.20.2

    .. seealso::

        :ref:`data_impact`

    """
    if isinstance(subject, MigrationScript):
        upgrade_direction = _direction_from_containers(
            "upgrade", subject.upgrade_ops_list
        )
        downgrade_direction = _direction_from_containers(
            "downgrade", subject.downgrade_ops_list
        )
        return DataImpactReport(upgrade_direction, downgrade_direction)
    elif isinstance(subject, UpgradeOps):
        upgrade_direction = _direction_from_containers("upgrade", [subject])
        downgrade_direction = DataImpactDirection(
            "downgrade", (), provided=False
        )
        return DataImpactReport(upgrade_direction, downgrade_direction)
    elif isinstance(subject, DowngradeOps):
        upgrade_direction = DataImpactDirection(
            "upgrade", (), provided=False
        )
        downgrade_direction = _direction_from_containers(
            "downgrade", [subject]
        )
        return DataImpactReport(upgrade_direction, downgrade_direction)
    elif isinstance(subject, OpContainer):
        upgrade_direction = _direction_from_containers("upgrade", [subject])
        downgrade_direction = DataImpactDirection(
            "downgrade", (), provided=False
        )
        return DataImpactReport(upgrade_direction, downgrade_direction)
    elif isinstance(subject, MigrateOperation):
        upgrade_direction = DataImpactDirection(
            "upgrade", tuple(_collect_impacts(subject)), provided=True
        )
        downgrade_direction = DataImpactDirection(
            "downgrade", (), provided=False
        )
        return DataImpactReport(upgrade_direction, downgrade_direction)
    else:
        raise TypeError(
            "inspect_data_impact() expects a MigrationScript, an "
            "OpContainer, or a MigrateOperation; got %r" % (subject,)
        )
