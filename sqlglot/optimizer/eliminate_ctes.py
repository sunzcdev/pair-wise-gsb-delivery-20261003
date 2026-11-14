from __future__ import annotations

import typing as t

from sqlglot.optimizer.helpers import (
    FunctionVolatilities,
    contains_volatile,
    normalize_function_volatilities,
)
from sqlglot.optimizer.journal import Journal, record
from sqlglot.optimizer.scope import Scope, build_scope


if t.TYPE_CHECKING:
    from sqlglot._typing import E


def eliminate_ctes(
    expression: E,
    journal: Journal | None = None,
    function_volatilities: FunctionVolatilities | None = None,
) -> E:
    """
    Remove unused CTEs from an expression.

    Example:
        >>> import sqlglot
        >>> sql = "WITH y AS (SELECT a FROM x) SELECT a FROM z"
        >>> expression = sqlglot.parse_one(sql)
        >>> eliminate_ctes(expression).sql()
        'SELECT a FROM z'

    Args:
        expression (sqlglot.Expr): expression to optimize
        function_volatilities: call-level volatility declarations for functions, keyed by
            function name; see `sqlglot.optimizer.helpers.normalize_function_volatilities`.
            CTEs holding per-call volatile functions (e.g. RAND) are kept even when
            unreferenced, since removing them would drop their evaluations.
    Returns:
        sqlglot.Expr: optimized expression
    """
    root = build_scope(expression)

    if root:
        ref_count = root.ref_count()
        volatilities = normalize_function_volatilities(function_volatilities)

        # Traverse the scope tree in reverse so we can remove chains of unused CTEs
        for scope in reversed(list(root.traverse())):
            if scope.is_cte:
                count = ref_count[id(scope)]
                if count <= 0 and not contains_volatile(scope.expression, volatilities):
                    cte_node = scope.expression.parent
                    if not cte_node:
                        continue
                    with_node = cte_node.parent
                    if journal is not None and with_node:
                        record(journal, with_node, "expressions")
                    cte_node.pop()

                    # Pop the entire WITH clause if this is the last CTE
                    if with_node and len(with_node.expressions) <= 0:
                        if journal is not None and with_node.parent:
                            record(journal, with_node.parent, "with_")
                        with_node.pop()

                    # Decrement the ref count for all sources this CTE selects from
                    for _, source in scope.selected_sources.values():
                        if isinstance(source, Scope):
                            ref_count[id(source)] -= 1

    return expression
