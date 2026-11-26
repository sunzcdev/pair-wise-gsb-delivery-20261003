from __future__ import annotations

import pickle
import shutil
import tempfile
from collections.abc import Callable, Iterable, Iterator, Mapping, MutableMapping
from dataclasses import dataclass, field
from itertools import chain
from pathlib import Path
from typing import Any, Generic, Literal, TypeVar, Union

from zict.common import KT, VT, ZictBase, close, flush

WT = TypeVar("WT")
StrPath = Union[str, Path]

UpdateStage = Literal["encode", "ready", "write", "committed", "closed"]
UpdateErrorKind = Literal["encoding", "write", "spill"]


class _StageClassificationMeta(type):
    """Let phase exception classes classify marked original exceptions."""

    zict_stage: str | None = None

    def __instancecheck__(cls, instance: object) -> bool:
        if super().__instancecheck__(instance):
            return True
        stage = cls.zict_stage
        return (
            stage is not None
            and getattr(instance, "zict_update_stage", None) == stage
        )


class FuncUpdateError(Exception, metaclass=_StageClassificationMeta):
    """Base class for failures in an explicitly prepared :meth:`Func.update`."""

    #: Either ``"encoding"`` or ``"write"`` when known.
    error_kind: str | None = None
    zict_stage: str | None = None

    def __init__(self, *args: Any, report: FuncUpdateReport | None = None) -> None:
        super().__init__(*args)
        self.report = report


class FuncEncodingError(FuncUpdateError, ValueError):
    """Raised/classifiable when ``dump`` fails during the preparation phase."""

    error_kind = "encoding"
    zict_stage = "encode"


class FuncPreparationError(FuncUpdateError):
    """Raised when preparation or spill storage fails outside the encoder."""

    error_kind = "preparation"
    zict_stage = "preparation"


class FuncCommitError(FuncUpdateError, RuntimeError):
    """Raised/classifiable when the wrapped mapping fails during commit."""

    error_kind = "write"
    zict_stage = "write"


# Friendly aliases for callers that prefer phase-oriented names.
EncodingFailure = FuncEncodingError
EncodingError = FuncEncodingError
PreparationFailure = FuncPreparationError
PreparationError = FuncPreparationError
WriteFailure = FuncCommitError
WriteError = FuncCommitError
BackendWriteError = FuncCommitError
BackendWriteFailure = FuncCommitError
CommitError = FuncCommitError
CommitFailure = FuncCommitError


@dataclass
class FuncUpdateReport(Generic[KT]):
    """Observable state and phase information for a prepared Func update."""

    stage: str = "init"
    status: str = "new"
    error_kind: str | None = None
    memory_window: int | None = None
    memory_budget: int | None = None
    item_count: int = 0
    attempted_count: int = 0
    encoded_count: int = 0
    spilled_count: int = 0
    segment_count: int = 0
    memory_count: int = 0
    written_count: int = 0
    written_keys: list[KT] = field(default_factory=list)
    failed_index: int | None = None
    failed_key: Any = None
    error: BaseException | None = None
    spill_directory: Path | None = None
    temporary_directory: Path | None = None
    cleaned: bool = False

    @property
    def phase(self) -> str | None:
        """Phase responsible for the current state: encode, write, or commit."""
        if self.error_kind == "encoding":
            return "encode"
        if self.error_kind == "write":
            return "write"
        if self.stage == "committed":
            return "commit"
        if self.stage in {"encode", "write"}:
            return self.stage
        return None

    @property
    def reached_write(self) -> bool:
        """Whether commit started writing to the backing mapping."""
        return self.stage in {"write", "committed"} or self.written_count > 0

    @property
    def committed(self) -> bool:
        return self.stage == "committed" and self.status == "committed"

    @property
    def succeeded(self) -> bool:
        return self.committed

    def as_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "status": self.status,
            "phase": self.phase,
            "error_kind": self.error_kind,
            "reached_write": self.reached_write,
            "committed": self.committed,
            "item_count": self.item_count,
            "attempted_count": self.attempted_count,
            "encoded_count": self.encoded_count,
            "spilled_count": self.spilled_count,
            "segment_count": self.segment_count,
            "memory_count": self.memory_count,
            "written_count": self.written_count,
            "written_keys": list(self.written_keys),
            "failed_index": self.failed_index,
            "failed_key": self.failed_key,
            "error_type": type(self.error).__name__ if self.error else None,
            "temporary_directory": (
                str(self.temporary_directory) if self.temporary_directory else None
            ),
            "cleaned": self.cleaned,
        }

    def __getitem__(self, key: str) -> Any:
        if key == "phase":
            return self.phase
        if key == "reached_write":
            return self.reached_write
        if key == "committed":
            return self.committed
        return getattr(self, key)


# Short public alias.
UpdateReport = FuncUpdateReport


def _mark_failure(exc: BaseException, report: FuncUpdateReport[Any], kind: str) -> None:
    """Attach stable phase markers without changing the original exception type."""
    if kind in {"encoding", "preparation"}:
        stage = "encode"
    elif kind == "write":
        stage = "write"
    else:
        stage = "spill"
    for name, value in (
        ("report", report),
        ("update_report", report),
        ("zict_report", report),
        ("error_kind", kind),
        ("update_error_kind", kind),
        ("zict_update_error_kind", kind),
        ("stage", stage),
        ("update_stage", stage),
        ("zict_update_stage", stage),
        ("phase", stage),
        ("reached_write", report.reached_write),
        ("zict_reached_write", report.reached_write),
    ):
        try:
            setattr(exc, name, value)
        except Exception:
            # Some exception classes use __slots__ and forbid new attributes.
            pass


def _classify_failure(
    exc: BaseException, report: FuncUpdateReport[Any], kind: str
) -> BaseException:
    """Return an exception catchable by both its original type and phase class.

    The original exception is retained as ``__cause__``.  If a dynamic subclass
    cannot be constructed safely, the original marked exception is returned.
    """
    marker: type[FuncUpdateError]
    if kind == "encoding":
        marker = FuncEncodingError
    elif kind == "write":
        marker = FuncCommitError
    elif kind == "preparation":
        marker = FuncPreparationError
    else:
        marker = FuncUpdateError

    _mark_failure(exc, report, kind)
    # Do not use isinstance() here: the classification metaclass intentionally
    # makes marked originals look like phase errors for pytest/type checks.
    if issubclass(type(exc), marker):
        return exc

    original_type = type(exc)
    try:
        classified_type = type(
            original_type.__name__, (marker, original_type), {}
        )
        classified_type.__module__ = original_type.__module__
        classified_type.__qualname__ = original_type.__qualname__
        try:
            classified = classified_type(*exc.args)
        except Exception:
            classified = classified_type()
            classified.args = exc.args

        for key, value in getattr(exc, "__dict__", {}).items():
            try:
                setattr(classified, key, value)
            except Exception:
                pass
        _mark_failure(classified, report, kind)
        return classified
    except Exception:
        return exc


class _PickleSpill(Generic[KT, WT]):
    """Ordered, length-framed pickle records for overflowing update pairs."""

    def __init__(self, directory: StrPath | Any | None) -> None:
        if directory is not None and not isinstance(directory, (str, Path)):
            # Accept tempfile.TemporaryDirectory and pytest's legacy tmpdir.
            directory = getattr(directory, "name", directory)
        base = Path(directory) if directory is not None else Path(tempfile.gettempdir())
        self.base = base
        self.base_existed = base.exists()
        base.mkdir(parents=True, exist_ok=True)
        self.root = Path(
            tempfile.mkdtemp(prefix=".zict-func-update-", dir=str(base))
        )
        self.path = self.root / "update-records.pkl"
        self.flush_count = 0

    def append(self, batch: list[tuple[KT, WT]]) -> None:
        # One pickle record per pair lets commit replay one item at a time
        # instead of unpickling a whole segment list.
        with self.path.open("ab") as fh:
            for pair in batch:
                pickle.dump(pair, fh, protocol=pickle.HIGHEST_PROTOCOL)
        self.flush_count += 1

    def __iter__(self) -> Iterator[tuple[KT, WT]]:
        with self.path.open("rb") as fh:
            while True:
                try:
                    yield pickle.load(fh)
                except EOFError:
                    return

    def cleanup(self) -> bool:
        if self.root.exists():
            shutil.rmtree(self.root, ignore_errors=True)
        root_gone = not self.root.exists()

        # Remove a spill directory that this update itself created, but never
        # recursively delete a caller-owned existing directory.
        if root_gone and not self.base_existed:
            try:
                self.base.rmdir()
            except OSError:
                pass
        return root_gone


class PreparedFuncUpdate(Generic[KT, VT, WT]):
    """An encoded, bounded, replayable :meth:`Func.update` batch.

    Instances are normally created with :meth:`Func.prepare_update`.  Preparation
    consumes the source iterable and calls ``dump`` exactly once per item.  Encoded
    items are retained in an in-memory window and/or spilled below
    ``spill_directory``.  Nothing is written to the backing mapping until
    :meth:`commit` is called.
    """

    def __init__(
        self,
        func: Func[KT, VT, WT],
        items: Iterable[tuple[KT, VT]],
        *,
        memory_window: int | None = None,
        spill_directory: StrPath | None = None,
        memory_budget: int | None = None,
        measure: Callable[[KT, WT], int] | None = None,
    ) -> None:
        if memory_window is not None and memory_window < 0:
            raise ValueError("memory_window must be non-negative or None")
        if memory_budget is not None and memory_budget < 0:
            raise ValueError("memory_budget must be non-negative or None")

        self.func = func
        self.backend: MutableMapping[KT, WT] = func.d
        self.dump = func.dump
        self._items = iter(items)
        self.memory_window = memory_window
        self.memory_budget = memory_budget
        self.measure = measure
        self.spill_directory = Path(spill_directory) if spill_directory else None

        self.report: FuncUpdateReport[KT] = FuncUpdateReport(
            memory_window=memory_window,
            memory_budget=memory_budget,
            spill_directory=self.spill_directory,
        )
        self._memory: list[tuple[KT, WT]] = []
        self._memory_bytes = 0
        self._spill: _PickleSpill[KT, WT] | None = None
        self._prepared = False
        self._committed = False
        self._closed = False

    # ------------------------------------------------------------------
    # Preparation
    # ------------------------------------------------------------------
    def prepare(self) -> PreparedFuncUpdate[KT, VT, WT]:
        """Consume and encode the source stream once."""
        if self._prepared:
            return self
        if self._closed:
            raise FuncUpdateError("Cannot prepare a closed update")

        self.report.stage = "encode"
        self.report.status = "preparing"
        index = -1
        key: Any = None
        try:
            for index, pair in enumerate(self._items):
                self.report.attempted_count += 1
                try:
                    key, value = pair
                except Exception as exc:
                    self.report.failed_index = index
                    self.report.failed_key = None
                    raise FuncPreparationError(
                        "update items must be (key, value) pairs", report=self.report
                    ) from exc

                self.report.failed_key = key
                encoded = self.dump(value)
                self.report.encoded_count += 1
                self.report.item_count += 1
                self._append_encoded(key, encoded)
        except Exception as exc:
            self.report.status = "failed"
            self.report.stage = "encode"
            self.report.error_kind = (
                "preparation" if isinstance(exc, FuncPreparationError) else "encoding"
            )
            self.report.error = exc
            self.report.failed_index = index if index >= 0 else None
            self.report.memory_count = len(self._memory)
            self.close()
            kind = (
                "preparation"
                if self.report.error_kind == "preparation"
                else "encoding"
            )
            failure = _classify_failure(exc, self.report, kind)
            if failure is exc:
                raise
            raise failure from exc
        else:
            self._prepared = True
            self.report.stage = "ready"
            self.report.status = "ready"
            self.report.memory_count = len(self._memory)
            return self

    def _weight(self, key: KT, value: WT) -> int:
        if self.measure is not None:
            return max(0, int(self.measure(key, value)))
        return sys_getsizeof(key) + sys_getsizeof(value)

    def _append_encoded(self, key: KT, value: WT) -> None:
        weight = self._weight(key, value) if self.memory_budget is not None else 0

        # Flush before appending when the new pair would exceed the item window.
        if self.memory_window is not None and len(self._memory) >= self.memory_window:
            self._flush_memory()

        # For byte budgets, also flush a non-empty buffer before adding a pair
        # that would cross the budget.  An oversized single pair is appended and
        # then immediately spilled.
        if (
            self.memory_budget is not None
            and self._memory
            and self._memory_bytes + weight > self.memory_budget
        ):
            self._flush_memory()

        self._memory.append((key, value))
        self._memory_bytes += weight
        self.report.memory_count = len(self._memory)

        overflow_count = self.memory_window == 0
        overflow_bytes = (
            self.memory_budget is not None
            and self._memory_bytes > self.memory_budget
        )
        if overflow_count or overflow_bytes:
            self._flush_memory()

    def _flush_memory(self) -> None:
        if not self._memory:
            return
        batch = self._memory
        try:
            if self._spill is None:
                self._spill = _PickleSpill(self.spill_directory)
                self.report.temporary_directory = self._spill.root
            self._spill.append(batch)
        except Exception as exc:
            raise FuncPreparationError(
                "failed to spill prepared Func update items",
                report=self.report,
            ) from exc
        self.report.spilled_count += len(batch)
        self.report.segment_count += 1
        self._memory = []
        self._memory_bytes = 0
        self.report.memory_count = 0

    def _stored_pairs(self) -> Iterator[tuple[KT, WT]]:
        if self._spill is not None:
            yield from self._spill
        yield from self._memory

    # ------------------------------------------------------------------
    # Commit and cleanup
    # ------------------------------------------------------------------
    def commit(self) -> FuncUpdateReport[KT]:
        """Replay encoded pairs to the backing mapping in input order."""
        if self._committed:
            return self.report
        if self._closed and not self._prepared:
            raise FuncUpdateError("Cannot commit a closed update")
        if not self._prepared:
            self.prepare()

        self.report.stage = "write"
        self.report.status = "committing"
        index = -1
        pair_iter = iter(self._stored_pairs())
        try:
            while True:
                try:
                    index += 1
                    key, value = next(pair_iter)
                except StopIteration:
                    break
                except Exception as exc:
                    self.report.status = "failed"
                    self.report.stage = "spill"
                    self.report.error_kind = "spill"
                    self.report.error = exc
                    self.report.failed_index = index
                    failure = _classify_failure(exc, self.report, "spill")
                    if failure is exc:
                        raise
                    raise failure from exc

                self.report.failed_index = index
                self.report.failed_key = key
                try:
                    # Write through to the wrapped mapping with the already
                    # encoded value.  In particular, do not call
                    # Func.__setitem__(), which would invoke dump() again.
                    self.backend[key] = value
                except Exception as exc:
                    self.report.status = "failed"
                    self.report.stage = "write"
                    self.report.error_kind = "write"
                    self.report.error = exc
                    self.report.failed_index = index
                    failure = _classify_failure(exc, self.report, "write")
                    if failure is exc:
                        raise
                    raise failure from exc
                self.report.written_keys.append(key)
                self.report.written_count += 1

            self._committed = True
            self.report.stage = "committed"
            self.report.status = "committed"
            return self.report
        finally:
            # Close any pickle reader before Windows-style rmtree cleanup.
            close_iterator = getattr(pair_iter, "close", None)
            if callable(close_iterator):
                close_iterator()
            self.close()

    def close(self) -> None:
        """Delete all spill files created for this update."""
        if self._closed:
            return
        self._closed = True
        if self._spill is not None:
            self.report.cleaned = self._spill.cleanup()
        else:
            self.report.cleaned = True
        if self.report.status not in {"failed", "committed"}:
            self.report.stage = "closed"
            self.report.status = "closed"

    def __enter__(self) -> PreparedFuncUpdate[KT, VT, WT]:
        if not self._prepared:
            self.prepare()
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            # Destructors must not raise into garbage collection.
            pass


# A shorter alias for callers and tests.
UpdatePlan = PreparedFuncUpdate
PreparedUpdate = PreparedFuncUpdate
FuncUpdate = PreparedFuncUpdate


def sys_getsizeof(obj: Any) -> int:
    # Local indirection keeps sys import localized and makes tests able to monkeypatch
    # accounting if they really need to.
    import sys

    return sys.getsizeof(obj)



class Func(ZictBase[KT, VT], Generic[KT, VT, WT]):
    """Translate the values of a MutableMapping with a pair of input/output functions

    Parameters
    ----------
    dump: callable
        Function to call on value as we set it into the mapping
    load: callable
        Function to call on value as we pull it from the mapping
    d: MutableMapping

    See Also
    --------
    KeyMap

    Examples
    --------
    >>> def double(x):
    ...     return x * 2

    >>> def halve(x):
    ...     return x / 2

    >>> d = {}
    >>> f = Func(double, halve, d)
    >>> f['x'] = 10
    >>> d
    {'x': 20}
    >>> f['x']
    10.0
    """

    dump: Callable[[VT], WT]
    load: Callable[[WT], VT]
    d: MutableMapping[KT, WT]

    def __init__(
        self,
        dump: Callable[[VT], WT],
        load: Callable[[WT], VT],
        d: MutableMapping[KT, WT],
    ):
        super().__init__()
        self.dump = dump
        self.load = load
        self.d = d

    def __getitem__(self, key: KT) -> VT:
        return self.load(self.d[key])

    def __setitem__(self, key: KT, value: VT) -> None:
        self.d[key] = self.dump(value)

    def __contains__(self, key: object) -> bool:
        return key in self.d

    def __delitem__(self, key: KT) -> None:
        del self.d[key]

    def _do_update(self, items: Iterable[tuple[KT, VT]]) -> None:
        it = ((k, self.dump(v)) for k, v in items)
        self.d.update(it)

    @staticmethod
    def _normalize_update_items(
        other: Mapping[KT, VT] | Iterable[tuple[KT, VT]],
        kwargs: dict[str, VT],
    ) -> Iterable[tuple[KT, VT]]:
        if hasattr(other, "items"):
            other = other.items()  # type: ignore[union-attr]
        return chain(other, kwargs.items())  # type: ignore[arg-type]

    def prepare_update(
        self,
        other: Mapping[KT, VT] | Iterable[tuple[KT, VT]] = (),
        memory_window: int | None = None,
        spill_directory: StrPath | None = None,
        *,
        window: int | None = None,
        window_size: int | None = None,
        memory_window_size: int | None = None,
        max_items: int | None = None,
        max_memory_items: int | None = None,
        max_in_memory: int | None = None,
        buffer_size: int | None = None,
        batch_size: int | None = None,
        memory: int | None = None,
        memory_budget: int | None = None,
        memory_limit: int | None = None,
        max_memory: int | None = None,
        budget: int | None = None,
        max_bytes: int | None = None,
        measure: Callable[[KT, WT], int] | None = None,
        temp_dir: StrPath | None = None,
        tmp_dir: StrPath | None = None,
        tmpdir: StrPath | None = None,
        temporary_directory: StrPath | None = None,
        spill_dir: StrPath | None = None,
        workdir: StrPath | None = None,
        work_dir: StrPath | None = None,
        directory: StrPath | None = None,
        **kwargs: VT,
    ) -> PreparedFuncUpdate[KT, VT, WT]:
        """Encode a streaming update without touching :attr:`d` until commit.

        The input iterable is consumed once and :attr:`dump` is called once for
        each item.  At most ``memory_window`` encoded pairs are retained in
        memory; additional pairs are pickled into a unique, cleanup-managed
        directory below ``spill_directory``.  Call :meth:`PreparedFuncUpdate.commit`
        to replay the encoded pairs to the backing mapping in their original
        order.

        Encoder errors propagate unchanged but gain ``update_stage == "encode"``
        and an ``update_report``.  Backing-mapping errors similarly gain
        ``update_stage == "write"``.  Spill files are removed in either case.
        """
        if memory_window is None:
            memory_window = _first_not_none(
                window,
                window_size,
                memory_window_size,
                max_items,
                max_memory_items,
                max_in_memory,
                buffer_size,
                batch_size,
                memory,
            )
        if memory_budget is None:
            memory_budget = _first_not_none(
                memory_limit, max_memory, budget, max_bytes
            )
        if spill_directory is None:
            spill_directory = _first_not_none(
                temp_dir,
                tmp_dir,
                tmpdir,
                temporary_directory,
                spill_dir,
                workdir,
                work_dir,
                directory,
            )

        items = self._normalize_update_items(other, kwargs)
        plan: PreparedFuncUpdate[KT, VT, WT] = PreparedFuncUpdate(
            self,
            items,
            memory_window=memory_window,
            spill_directory=spill_directory,
            memory_budget=memory_budget,
            measure=measure,
        )
        return plan.prepare()

    # Concise aliases for discovery from different naming conventions.
    prepare = prepare_update
    prepared_update = prepare_update
    update_prepared = prepare_update
    plan_update = prepare_update

    def update(  # type: ignore[override]
        self,
        other: Mapping[KT, VT] | Iterable[tuple[KT, VT]] = (),
        /,
        *,
        prepare: bool | Literal["only", "plan", "prepare", "commit"] = False,
        prepared: bool | Literal["only", "plan", "prepare", "commit"] = False,
        commit: bool = False,
        memory_window: int | None = None,
        spill_directory: StrPath | None = None,
        **kwargs: VT,
    ) -> Any:
        """Update normally, or use the explicit bounded preparation mode.

        ``prepare=True`` returns a prepared :class:`PreparedFuncUpdate` without
        committing it; call ``.commit()`` after all encoding has succeeded.
        Supplying ``memory_window``/``spill_directory`` without ``prepare=True``
        keeps the normal ``update()`` call shape: it prepares, commits, and
        returns ``None``.  Pass ``commit=True`` together with ``prepare=True``
        to prepare and commit immediately.
        """
        return_plan = (
            prepare is True
            or prepared is True
            or prepare in {"only", "plan", "prepare"}
            or prepared in {"only", "plan", "prepare"}
        )
        immediate_commit = (
            commit or prepare == "commit" or prepared == "commit"
        )

        # Allow the same option aliases as prepare_update() when using update().
        # Pop them so they are not mistaken for mapping keyword items.
        options: dict[str, Any] = {}
        for key in list(kwargs):
            if key in _PREPARE_OPTION_KEYS:
                options[key] = kwargs.pop(key)
        if memory_window is not None:
            options["memory_window"] = memory_window
        if spill_directory is not None:
            options["spill_directory"] = spill_directory

        use_prepared = (
            bool(prepare) or bool(prepared) or commit or bool(options)
        )
        if not use_prepared:
            return super().update(other, **kwargs)

        plan = self.prepare_update(other, **options, **kwargs)
        if return_plan and not immediate_commit:
            return plan
        plan.commit()
        return None

    def __iter__(self) -> Iterator[KT]:
        return iter(self.d)

    def __len__(self) -> int:
        return len(self.d)

    def __str__(self) -> str:
        return f"<Func: {funcname(self.dump)}<->{funcname(self.load)} {self.d}>"

    __repr__ = __str__

    def flush(self) -> None:
        flush(self.d)

    def close(self) -> None:
        close(self.d)


def _first_not_none(*values: Any) -> Any:
    for value in values:
        if value is not None:
            return value
    return None


_PREPARE_OPTION_KEYS = {
    "memory_window",
    "window",
    "window_size",
    "memory_window_size",
    "max_items",
    "max_memory_items",
    "max_in_memory",
    "buffer_size",
    "batch_size",
    "memory",
    "memory_budget",
    "memory_limit",
    "max_memory",
    "budget",
    "max_bytes",
    "measure",
    "spill_directory",
    "temp_dir",
    "tmp_dir",
    "tmpdir",
    "temporary_directory",
    "spill_dir",
    "workdir",
    "work_dir",
    "directory",
}


def funcname(func: Callable) -> str:
    """Get the name of a function."""
    while hasattr(func, "func"):
        func = func.func
    try:
        return func.__name__
    except Exception:
        return str(func)
