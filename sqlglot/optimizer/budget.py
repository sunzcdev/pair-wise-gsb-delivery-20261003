from __future__ import annotations

import typing as t

from sqlglot.errors import OptimizeBudgetExceeded

if t.TYPE_CHECKING:
    from sqlglot import exp


class OptimizerBudget:
    """A deterministic work budget for the optimizer.

    Work is counted in interpretable units rather than wall-clock time: one unit
    roughly corresponds to one expression node visited or produced while a rule
    runs. Each rule is charged one unit per node in the tree when it starts, and
    rules that can expand the tree (e.g. boolean normalization) consume additional
    units as they process nodes, so combinatorial explosions are stopped promptly.

    Args:
        max_work: the maximum number of work units to allow. ``None`` means unlimited.
        input_mutated: whether the input expression may be partially mutated if the
            budget is exhausted mid-rule (i.e. the optimizer runs with ``copy=False``).
    """

    def __init__(self, max_work: int | None = None, input_mutated: bool = False):
        self.max_work = max_work
        self.consumed = 0
        self.completed_rules: list[str] = []
        self.current_rule: str | None = None
        self.input_mutated = input_mutated

    @property
    def enabled(self) -> bool:
        return self.max_work is not None

    def start_rule(self, name: str, expression: exp.Expr) -> None:
        self.current_rule = name
        if self.enabled:
            self.consume(sum(1 for _ in expression.walk()))

    def finish_rule(self) -> None:
        self.completed_rules.append(t.cast(str, self.current_rule))

    def consume(self, units: int = 1) -> None:
        if not self.enabled:
            return

        self.consumed += units

        if self.consumed > t.cast(int, self.max_work):
            raise OptimizeBudgetExceeded(
                f"Optimizer work budget of {self.max_work} units exceeded "
                f"while running rule '{self.current_rule}'",
                budget=self.max_work,
                consumed=self.consumed,
                completed_rules=self.completed_rules,
                stopped_at=self.current_rule,
                input_mutated=self.input_mutated,
            )
