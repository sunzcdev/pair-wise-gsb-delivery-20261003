# ruff: noqa: F401
"""
.. include:: ../README.md

----
"""

from __future__ import annotations

from collections.abc import Collection
from builtins import type as Type
from dataclasses import dataclass, field

import logging
import typing as t

from sqlglot import expressions as exp
from sqlglot.dialects.dialect import Dialect as Dialect, Dialects as Dialects
from sqlglot.diff import diff as diff
from sqlglot.errors import (
    ErrorLevel as ErrorLevel,
    ParseError as ParseError,
    TokenError as TokenError,
    TranspileIssue as TranspileIssue,
    UnsupportedError as UnsupportedError,
)
from sqlglot.expressions import (
    Expr as Expr,
    alias_ as alias,
    and_ as and_,
    case as case,
    cast as cast,
    column as column,
    condition as condition,
    delete as delete,
    except_ as except_,
    find_tables as find_tables,
    from_ as from_,
    func as func,
    insert as insert,
    intersect as intersect,
    maybe_parse as maybe_parse,
    merge as merge,
    not_ as not_,
    or_ as or_,
    select as select,
    subquery as subquery,
    table_ as table,
    to_column as to_column,
    to_identifier as to_identifier,
    to_table as to_table,
    union as union,
)
from sqlglot.generator import Generator as Generator
from sqlglot.parser import Parser as Parser
from sqlglot.schema import MappingSchema as MappingSchema, Schema as Schema
from sqlglot.tokens import Token as Token, Tokenizer as Tokenizer, TokenType as TokenType

if t.TYPE_CHECKING:
    from sqlglot._typing import E, GeneratorArgs, ParserNoDialectArgs
    from typing_extensions import Unpack
    from sqlglot.dialects.dialect import DialectType as DialectType

logger = logging.getLogger("sqlglot")


try:
    from sqlglot._version import __version__, __version_tuple__  # type: ignore[import-not-found]
except ImportError:
    logger.error(
        "Unable to set __version__, run `pip install -e .` or `python setup.py develop` first."
    )


pretty = False
"""Whether to format generated SQL by default."""


@dataclass
class TranspileResult:
    """The outcome of transpiling a single SQL statement in issue-collection mode.

    Attributes:
        sql: The transpiled SQL statement, or None if `strict` mode was requested and
            semantic losses were found, in which case no executable SQL is returned.
        issues: The structured semantic losses encountered while transpiling the statement.
    """

    sql: str | None
    issues: list[TranspileIssue] = field(default_factory=list)


def tokenize(sql: str, read: DialectType = None, dialect: DialectType = None) -> list[Token]:
    """
    Tokenizes the given SQL string.

    Args:
        sql: the SQL code string to tokenize.
        read: the SQL dialect to apply during tokenizing (eg. "spark", "hive", "presto", "mysql").
            See `sqlglot.dialects.Dialects` for options.
        dialect: the SQL dialect (alias for read).

    Returns:
        The resulting list of tokens.
    """
    return Dialect.get_or_raise(read or dialect).tokenize(sql)


def parse(
    sql: str,
    read: DialectType = None,
    dialect: DialectType = None,
    **opts: Unpack[ParserNoDialectArgs],
) -> list[Expr | None]:
    """
    Parses the given SQL string into a collection of syntax trees, one per parsed SQL statement.

    Args:
        sql: the SQL code string to parse.
        read: the SQL dialect to apply during parsing (eg. "spark", "hive", "presto", "mysql").
            See `sqlglot.dialects.Dialects` for options.
        dialect: the SQL dialect (alias for read).
        **opts: other `sqlglot.parser.Parser` options.

    Returns:
        The resulting syntax tree collection.
    """
    return Dialect.get_or_raise(read or dialect).parse(sql, **opts)


@t.overload
def parse_one(
    sql: str,
    *,
    read: DialectType = ...,
    dialect: DialectType = ...,
    into: Type[E],
    **opts: Unpack[ParserNoDialectArgs],
) -> E: ...


@t.overload
def parse_one(
    sql: str,
    read: DialectType = ...,
    dialect: DialectType = ...,
    into: exp.IntoType | None = ...,
    **opts: Unpack[ParserNoDialectArgs],
) -> Expr: ...


def parse_one(
    sql: str,
    read: DialectType = None,
    dialect: DialectType = None,
    into: exp.IntoType | None = None,
    **opts: Unpack[ParserNoDialectArgs],
) -> Expr:
    """
    Parses the given SQL string and returns a syntax tree.

    Args:
        sql: the SQL code string to parse.
        read: the SQL dialect to apply during parsing (eg. "spark", "hive", "presto", "mysql").
            See `sqlglot.dialects.Dialects` for options.
        dialect: the SQL dialect (alias for read)
        into: the SQLGlot Expr to parse into.
        **opts: other `sqlglot.parser.Parser` options.

    Returns:
        A single syntax tree if one statement is parsed, otherwise a Block expression containing all parsed syntax trees.
    """

    dialect = Dialect.get_or_raise(read or dialect)

    if into:
        result = dialect.parse_into(into, sql, **opts)
    else:
        result = dialect.parse(sql, **opts)

    if not result or result[0] is None:
        raise ParseError(f"No expression was parsed from '{sql}'")

    return exp.Block(expressions=result) if len(result) > 1 else result[0]


def transpile(
    sql: str,
    read: DialectType = None,
    write: DialectType = None,
    identity: bool = True,
    error_level: ErrorLevel | None = None,
    **opts: Unpack[GeneratorArgs],
) -> list[str]:
    """
    Parses the given SQL string in accordance with the source dialect and returns a list of SQL strings transformed
    to conform to the target dialect. Each string in the returned list represents a single transformed SQL statement.

    Args:
        sql: the SQL code string to transpile.
        read: the source dialect used to parse the input string (eg. "spark", "hive", "presto", "mysql").
            See `sqlglot.dialects.Dialects` for options.
        write: the target dialect into which the input should be transformed (eg. "spark", "hive", "presto", "mysql").
            See `sqlglot.dialects.Dialects` for options.
        identity: if set to `True` and if the target dialect is not specified the source dialect will be used as both:
            the source and the target dialect.
        error_level: the desired error level of the parser.
        **opts: other `sqlglot.generator.Generator` options.

    Returns:
        The list of transpiled SQL statements.
    """
    write = (read if write is None else write) if identity else write
    write = Dialect.get_or_raise(write)
    return [
        write.generate(expression, copy=False, **opts) if expression else ""
        for expression in parse(sql, read, error_level=error_level)
    ]


def transpile_with_issues(
    sql: str,
    read: DialectType = None,
    write: DialectType = None,
    identity: bool = True,
    error_level: ErrorLevel | None = None,
    strict: bool = False,
    **opts: Unpack[GeneratorArgs],
) -> list[TranspileResult]:
    """
    Transpiles the given SQL string like `transpile`, but also collects every semantic loss
    (dropped, approximated, or unexpressible constructs) as a structured `TranspileIssue`,
    so callers can decide whether the result is safe to ship.

    Args:
        sql: the SQL code string to transpile.
        read: the source dialect used to parse the input string (eg. "spark", "hive", "presto", "mysql").
            See `sqlglot.dialects.Dialects` for options.
        write: the target dialect into which the input should be transformed (eg. "spark", "hive",
            "presto", "mysql"). See `sqlglot.dialects.Dialects` for options.
        identity: if set to `True` and if the target dialect is not specified the source dialect will
            be used as both: the source and the target dialect.
        error_level: the desired error level of the parser.
        strict: if set to `True` and any semantic loss was collected for a statement, the
            corresponding result's `sql` is None instead of executable SQL.
        **opts: other `sqlglot.generator.Generator` options.

    Returns:
        A list of `TranspileResult`, one per transpiled statement.
    """
    write = (read if write is None else write) if identity else write
    write = Dialect.get_or_raise(write)

    results = []
    for expression in parse(sql, read, error_level=error_level):
        if not expression:
            results.append(TranspileResult(sql=""))
            continue

        generator = write.generator(collect_issues=True, **opts)
        out = generator.generate(expression, copy=False)
        issues = generator.unsupported_issues
        results.append(TranspileResult(sql=None if strict and issues else out, issues=issues))

    return results
