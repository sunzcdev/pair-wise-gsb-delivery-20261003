from __future__ import annotations

import typing as t
from dataclasses import dataclass, field

from sqlglot import exp
from sqlglot.dialects.dialect import DialectType

if t.TYPE_CHECKING:
    from sqlglot.optimizer.optimizer import OptimizerFn

# Hard caps so a long optimization pipeline can never grow a trace without bound.
DEFAULT_MAX_ENTRIES: t.Final = 1000
DEFAULT_MAX_FRAGMENT_CHARS: t.Final = 100_000


@dataclass
class OptimizerTraceEntry:
    """A single optimizer rule application, recorded in execution order.

    Attributes:
        rule: Name of the rule that was applied (``rule.__name__``).
        changed: Whether the rule structurally changed the expression tree.
        before_sql: SQL fragment of the tree before the rule, or ``None`` when the
            fragment budget was exhausted before this entry was rendered.
        after_sql: SQL fragment of the tree after the rule, or ``None``.
        affected_nodes: Keys of the nodes that differ, ordered by their pre-order
            position in the tree. Empty when nothing changed.
        error: Message of the exception raised by the rule, if it failed.
        truncated: Whether this entry's fragments were shortened to fit the limits.
    """

    rule: str
    changed: bool
    before_sql: str | None = None
    after_sql: str | None = None
    affected_nodes: list[str] = field(default_factory=list)
    error: str | None = None
    truncated: bool = False


class OptimizerTrace:
    """Collects a step-by-step trace of an :func:`~sqlglot.optimizer.optimize` run.

    Pass an instance as ``trace`` to ``optimize``. Every rule application appends an
    :class:`OptimizerTraceEntry`, so the same rule can appear multiple times and rules
    that do nothing remain distinguishable through their ``changed`` flag.

    Fragments are rendered to SQL strings at record time, so they stay stable even
    though the underlying AST keeps being mutated in place.

    Args:
        max_entries: Maximum number of entries kept. Entries beyond this limit are
            dropped and ``records_truncated`` is set.
        max_fragment_chars: Maximum total length (in characters) of all stored SQL
            fragments. Once exceeded, later entries keep their metadata but store no
            fragments, and ``fragments_truncated`` is set.
    """

    def __init__(
        self,
        max_entries: int = DEFAULT_MAX_ENTRIES,
        max_fragment_chars: int = DEFAULT_MAX_FRAGMENT_CHARS,
    ) -> None:
        self.max_entries = max_entries
        self.max_fragment_chars = max_fragment_chars
        self.entries: list[OptimizerTraceEntry] = []
        self.records_truncated = False
        self.fragments_truncated = False
        self._fragment_chars = 0
        # Pending "before" state for the rule currently running. The tree is deep-copied
        # eagerly so neither the snapshot nor the change comparison can drift when the
        # rule mutates the working tree in place. The copy only happens while tracing.
        self._pending_before_sql: str | None = None
        self._pending_before_tree: exp.Expr | None = None
        self._pending_truncated = False

    def __iter__(self) -> t.Iterator[OptimizerTraceEntry]:
        return iter(self.entries)

    def __len__(self) -> int:
        return len(self.entries)

    @property
    def truncated(self) -> bool:
        """Whether any entry or fragment was dropped due to the configured limits."""
        return self.records_truncated or self.fragments_truncated

    def begin(self, before: exp.Expr, dialect: DialectType = None) -> None:
        """Captures the state of the tree immediately before a rule runs."""
        snapshot = before.copy()
        self._pending_before_tree = snapshot
        self._pending_before_sql = self._render(snapshot, dialect)
        self._pending_truncated = self.fragments_truncated

    def fail(self, rule: OptimizerFn, error: BaseException) -> None:
        """Records a rule that raised, without any after-state, then clears pending state."""
        before_sql = self._pending_before_sql
        truncated = self._pending_truncated
        self._reset_pending()

        if len(self.entries) >= self.max_entries:
            self.records_truncated = True
            return

        self.entries.append(
            OptimizerTraceEntry(
                rule=self._rule_name(rule),
                changed=False,
                before_sql=before_sql,
                error=str(error),
                truncated=truncated,
            )
        )

    def record(
        self,
        rule: OptimizerFn,
        before: exp.Expr,
        after: exp.Expr,
        dialect: DialectType = None,
    ) -> None:
        """Records one completed rule application and clears pending state."""
        before_sql = self._pending_before_sql
        before_tree = self._pending_before_tree
        truncated = self._pending_truncated
        self._reset_pending()

        if len(self.entries) >= self.max_entries:
            self.records_truncated = True
            return

        assert before_tree is not None
        changed = hash(before_tree) != hash(after)
        affected = _diff_node_keys(before_tree, after) if changed else []

        entry = OptimizerTraceEntry(
            rule=self._rule_name(rule),
            changed=changed,
            before_sql=before_sql,
            affected_nodes=affected,
            truncated=truncated,
        )

        # "after" is rendered now that the rule has finished; "before" was captured eagerly.
        if changed:
            after_sql = self._render(after, dialect)
            entry.after_sql = after_sql
            entry.truncated = truncated or self.fragments_truncated
        elif before_sql is not None:
            # No-op rules stay identifiable through `changed`; reuse the stable before
            # fragment so the entry is self-contained without rendering the tree again.
            entry.after_sql = before_sql

        self.entries.append(entry)

    def _reset_pending(self) -> None:
        self._pending_before_sql = None
        self._pending_before_tree = None
        self._pending_truncated = False

    @staticmethod
    def _rule_name(rule: OptimizerFn) -> str:
        return getattr(rule, "__name__", type(rule).__name__)

    def _render(self, expression: exp.Expr, dialect: DialectType) -> str | None:
        if self._fragment_chars >= self.max_fragment_chars:
            self.fragments_truncated = True
            return None

        sql = expression.sql(dialect=dialect)
        budget = self.max_fragment_chars - self._fragment_chars
        if len(sql) > budget:
            sql = _truncate(sql, budget)
            self.fragments_truncated = True

        self._fragment_chars += len(sql)
        return sql


def _truncate(text: str, limit: int) -> str:
    if limit <= 0:
        return ""
    if len(text) <= limit:
        return text
    return text[: max(0, limit - 3)] + "..."


def _diff_node_keys(before: exp.Expr, after: exp.Expr) -> list[str]:
    """Returns the keys of node pairs that differ between two same-shaped trees.

    The two trees are walked side by side, pairing each node with its positional
    counterpart. When a pair differs and its direct children stay aligned, the walk
    descends so callers see the deepest affected subtrees; when the child count
    diverges (a node was inserted or removed) the pair itself is reported and not
    descended into, since its subtrees can no longer be aligned.
    """
    affected: list[str] = []
    stack: list[tuple[exp.Expr, exp.Expr]] = [(before, after)]

    while stack:
        left, right = stack.pop()

        if hash(left) == hash(right):
            continue

        left_children = list(left.iter_expressions())
        right_children = list(right.iter_expressions())
        if len(left_children) != len(right_children) or left.key != right.key:
            affected.append(right.key)
            continue

        changed_children = [
            (l_child, r_child)
            for l_child, r_child in zip(left_children, right_children)
            if hash(l_child) != hash(r_child)
        ]
        if not changed_children:
            affected.append(right.key)
        else:
            for child_pair in reversed(changed_children):
                stack.append(child_pair)

    return affected
