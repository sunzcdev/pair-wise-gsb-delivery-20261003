"""
    tablib.references
    ~~~~~~~~~~~~~~~~~

    Inter-table references for :class:`tablib.Databook`.

    A reference declares that a column combination of a source sheet
    corresponds to a unique column combination of a target sheet, much like
    a foreign key -- but resolved purely in memory, without any database
    service.

    References are bound to the *identity* of the :class:`tablib.Dataset`
    objects inside the book, never to their titles: two sheets sharing the
    same title stay unambiguous.  Cell values are compared in their original
    Python type (``1`` never matches ``"1"`` or ``1.0``), and the policy for
    missing (``None``) key values must be stated explicitly.

    :copyright: (c) 2019 Jazzband.
    :license: MIT, see LICENSE for more details.
"""

from collections import namedtuple

from .exceptions import InvalidMissingPolicy

#: Rows whose key contains a missing value are excluded from verification.
MISSING_SKIP = 'skip'

#: Rows whose key contains a missing value are reported as violations.
MISSING_ERROR = 'error'

MISSING_POLICIES = (MISSING_SKIP, MISSING_ERROR)

#: A value counts as "missing" when it is None.
MISSING_VALUE = None

MissingTable = namedtuple('MissingTable', ['reference', 'role'])
MissingColumn = namedtuple('MissingColumn', ['reference', 'role', 'column'])
DuplicateTarget = namedtuple('DuplicateTarget', ['reference', 'key', 'count'])
DanglingReference = namedtuple('DanglingReference', ['reference', 'row', 'key'])
MissingValue = namedtuple('MissingValue', ['reference', 'row', 'key'])


class _CellKey:
    """Hashable wrapper comparing a cell by its original type *and* value."""

    __slots__ = ('type', 'value')

    def __init__(self, value):
        self.type = type(value)
        self.value = value

    def __eq__(self, other):
        return self.type is other.type and self.value == other.value

    def __hash__(self):
        try:
            return hash((self.type, self.value))
        except TypeError:
            # Unhashable cell (e.g. a list): fall back to its repr so that
            # verification keeps working instead of blowing up.
            return hash((self.type, repr(self.value)))


def _typed_key(values):
    return tuple(_CellKey(v) for v in values)


class Reference:
    """A directed reference between two sheets of a :class:`tablib.Databook`.

    :param source: the :class:`tablib.Dataset` holding the referring columns.
    :param source_columns: column headers of the source key, in order.
    :param target: the :class:`tablib.Dataset` holding the referenced columns.
    :param target_columns: column headers of the target key, in order; the
        combination is expected to be unique in the target sheet.
    :param missing: required missing-value policy, either
        ``tablib.references.MISSING_SKIP`` or
        ``tablib.references.MISSING_ERROR``.
    :param name: optional human-readable label for reports.
    """

    def __init__(self, source, source_columns, target, target_columns, *,
                 missing, name=None):
        if missing not in MISSING_POLICIES:
            raise InvalidMissingPolicy(
                f"missing must be one of {MISSING_POLICIES!r}, "
                f"got {missing!r}"
            )
        source_columns = tuple(source_columns)
        target_columns = tuple(target_columns)
        if not source_columns:
            raise ValueError('a reference needs at least one source column')
        if len(source_columns) != len(target_columns):
            raise ValueError(
                'source and target column combinations must have the '
                'same arity'
            )
        self.source = source
        self.source_columns = source_columns
        self.target = target
        self.target_columns = target_columns
        self.missing = missing
        self.name = name
        # Metadata snapshots, kept in sync by Databook.rename_sheet().
        self._source_title = getattr(source, 'title', None)
        self._target_title = getattr(target, 'title', None)

    def __repr__(self):
        return (
            f'<reference {self.label} '
            f'{self.source_columns!r} -> {self.target_columns!r}>'
        )

    @property
    def label(self):
        if self.name:
            return self.name
        return f'{self._source_title!r} -> {self._target_title!r}'

    @property
    def source_title(self):
        """Last known title of the source sheet (metadata snapshot)."""
        return self._source_title

    @property
    def target_title(self):
        """Last known title of the target sheet (metadata snapshot)."""
        return self._target_title

    def involves(self, dataset):
        """Whether this reference is bound to the given sheet identity."""
        return self.source is dataset or self.target is dataset

    def _sync_title(self, dataset, new_title):
        """Update the metadata snapshot after a controlled rename."""
        if self.source is dataset:
            self._source_title = new_title
        if self.target is dataset:
            self._target_title = new_title


class ReferenceReport:
    """Read-only outcome of :meth:`tablib.Databook.verify_references`.

    Issues are grouped in five categories:

    - ``missing_tables``: sheets a reference points to that are no longer
      members of the book;
    - ``missing_columns``: key columns absent from a sheet's headers;
    - ``duplicate_targets``: target key combinations that are not unique;
    - ``dangling``: source keys with no counterpart in the target sheet;
    - ``missing_values``: source keys containing ``None`` while the
      reference's policy is ``'error'``.
    """

    def __init__(self):
        self.missing_tables = []
        self.missing_columns = []
        self.duplicate_targets = []
        self.dangling = []
        self.missing_values = []

    @property
    def ok(self):
        """True when no issue was found in any category."""
        return not any((
            self.missing_tables,
            self.missing_columns,
            self.duplicate_targets,
            self.dangling,
            self.missing_values,
        ))

    def __bool__(self):
        return self.ok

    def _add(self, category, issue):
        getattr(self, category).append(issue)

    def summary(self):
        """A human-readable, one-line-per-issue summary."""
        if self.ok:
            return 'all references verified OK'
        lines = []
        for issue in self.missing_tables:
            lines.append(
                f'missing table: {issue.reference.label} ({issue.role})'
            )
        for issue in self.missing_columns:
            lines.append(
                f'missing column: {issue.reference.label} '
                f'({issue.role}) {issue.column!r}'
            )
        for issue in self.duplicate_targets:
            lines.append(
                f'duplicate target: {issue.reference.label} '
                f'key={issue.key!r} x{issue.count}'
            )
        for issue in self.dangling:
            lines.append(
                f'dangling reference: {issue.reference.label} '
                f'row={issue.row} key={issue.key!r}'
            )
        for issue in self.missing_values:
            lines.append(
                f'missing value: {issue.reference.label} '
                f'row={issue.row} key={issue.key!r}'
            )
        return '\n'.join(lines)

    def __repr__(self):
        status = 'ok' if self.ok else 'issues found'
        return f'<reference report: {status}>'


def _resolve_columns(reference, dataset, columns, role, report):
    """Map header names to positions, reporting missing ones.

    Returns the list of column indexes, or None when unresolvable.
    """
    headers = dataset.headers
    if not headers:
        for column in columns:
            report._add('missing_columns', MissingColumn(reference, role, column))
        return None
    indexes = []
    complete = True
    for column in columns:
        if column in headers:
            indexes.append(headers.index(column))
        else:
            report._add('missing_columns', MissingColumn(reference, role, column))
            complete = False
    return indexes if complete else None


def verify_reference(reference, datasets, report):
    """Check a single reference against the book's sheets, read-only."""
    source_present = any(ds is reference.source for ds in datasets)
    target_present = any(ds is reference.target for ds in datasets)
    if not source_present:
        report._add('missing_tables', MissingTable(reference, 'source'))
    if not target_present:
        report._add('missing_tables', MissingTable(reference, 'target'))

    source_idx = target_idx = None
    if source_present:
        source_idx = _resolve_columns(
            reference, reference.source, reference.source_columns,
            'source', report,
        )
    if target_present:
        target_idx = _resolve_columns(
            reference, reference.target, reference.target_columns,
            'target', report,
        )
    if source_idx is None or target_idx is None:
        return

    # Uniqueness of the target key combination.
    counts = {}
    for row in reference.target:
        key = _typed_key(row[i] for i in target_idx)
        counts[key] = counts.get(key, 0) + 1
    for key, count in counts.items():
        if count > 1:
            report._add(
                'duplicate_targets',
                DuplicateTarget(
                    reference, tuple(cell.value for cell in key), count,
                ),
            )

    target_keys = set(counts)
    for row_number, row in enumerate(reference.source):
        values = tuple(row[i] for i in source_idx)
        if any(value is MISSING_VALUE for value in values):
            if reference.missing == MISSING_ERROR:
                report._add(
                    'missing_values',
                    MissingValue(reference, row_number, values),
                )
            # MISSING_SKIP: the row is excluded from dangling checks.
            continue
        if _typed_key(values) not in target_keys:
            report._add(
                'dangling',
                DanglingReference(reference, row_number, values),
            )
