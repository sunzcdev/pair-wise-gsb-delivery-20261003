import logging
import threading
from collections import deque
from contextvars import ContextVar

logger = logging.getLogger("fsspec")

# Per-context stacks of active transactions, keyed by the filesystem they run
# on. Filesystem instances are cached and therefore shared between threads and
# async contexts, so transaction state cannot live on the instance itself:
# every execution context gets its own stack through this context variable.
# The default must be None rather than a shared dict: mutating a mutable
# default before calling ``.set()`` would leak into every other context.
_active_transactions: ContextVar = ContextVar(
    "fsspec_active_transactions", default=None
)
# Stable identity of the current context, used to mark the context that owns a
# given transaction. Lazily initialised so a context only gets an id when a
# transaction is actually started in it.
_context_id: ContextVar = ContextVar("fsspec_context_id", default=None)
# Process-wide counter so ids stay unique across threads; a per-thread counter
# would restart at 1 in every new thread and collide with an earlier owner.
_context_id_counter = 0
_context_id_lock = threading.Lock()


def _current_context_id():
    global _context_id_counter
    ident = _context_id.get()
    if ident is None:
        with _context_id_lock:
            _context_id_counter += 1
            ident = _context_id_counter
        _context_id.set(ident)
    return ident


def _transaction_stacks():
    """Return the mapping of filesystem -> active transaction stack.

    The mapping is created lazily inside the current context, so different
    threads and copied ``contextvars`` contexts never share one.
    """
    stacks = _active_transactions.get()
    if stacks is None:
        stacks = {}
        _active_transactions.set(stacks)
    return stacks


def active_transaction(fs):
    """Return the transaction currently active for ``fs`` in this context.

    Returns ``None`` when no transaction is active here, even if another
    context has one open on the same (cached) filesystem instance.
    """
    stacks = _active_transactions.get()
    if not stacks:
        return None
    stack = stacks.get(fs)
    if stack:
        return stack[-1]
    return None


class Transaction:
    """Filesystem transaction write context

    Gathers files for deferred commit or discard, so that several write
    operations can be finalized semi-atomically.

    Transaction state is kept per execution context rather than on the
    filesystem instance. Cached filesystem instances are shared between
    threads (and ``contextvars`` contexts), so a transaction started in one
    context only defers writes made through that context: ordinary writes from
    another context on the same instance are still committed immediately, and
    each transaction owns the queue of files it created.

    Nesting rules within one context:

    * re-entering the same transaction (including ``fs.transaction`` while it
      is active) joins the existing transaction, and only the outermost entry
      commits or discards the files;
    * starting a *different* transaction while one is already active raises
      ``RuntimeError``.

    Ending a transaction from a context other than the one that started it is
    also rejected, instead of mutating state the owning context still uses.
    """

    def __init__(self, fs, **kwargs):
        """
        Parameters
        ----------
        fs: FileSystem instance
        """
        self.fs = fs
        self.files = deque()
        # set on the first start(); identifies the owning context
        self._owner_id = None
        # number of (re-entrant) entries still open in the owning context
        self._depth = 0
        self._completed = False
        self.invalidated_caches = []

    def __enter__(self):
        self.start()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        """End transaction and commit, if exit is not due to exception.

        Only the outermost exit finalises the files; inner, re-entrant exits
        merely unwind the nesting.
        """
        # never swallow the exception that triggered the rollback
        self._unwind(commit=exc_type is None)
        return False

    def start(self):
        """Start a transaction on this FileSystem in the current context"""
        stacks = _transaction_stacks()
        stack = stacks.setdefault(self.fs, [])
        if stack:
            if stack[-1] is self:
                # re-entrant use of the same transaction, e.g. nested
                # ``with fs.transaction:`` blocks
                self._depth += 1
                return
            raise RuntimeError(
                "A transaction is already active on this filesystem in the "
                "current context; finish it before starting another one."
            )
        if self._owner_id is not None:
            raise RuntimeError(
                "This transaction belongs to another context and cannot be "
                "reused here; open a new transaction in this context instead."
            )
        self.files = deque()  # clean up after previous failed completions
        self.invalidated_caches = []
        self._completed = False
        self._owner_id = _current_context_id()
        self._depth = 1
        stack.append(self)

    def _require_owner(self):
        """Raise unless this call runs in the context that owns this transaction"""
        owner_id = self._owner_id
        if owner_id is None:
            raise RuntimeError("This transaction has not been started in any context.")
        if owner_id != _current_context_id():
            raise RuntimeError(
                "Cannot finish a transaction from a different context than "
                "the one that started it."
            )

    def _require_own_stack(self):
        """Return this context's stack for the fs, rejecting foreign contexts"""
        self._require_owner()
        stack = _transaction_stacks().get(self.fs)
        if not stack or self not in stack:
            raise RuntimeError(
                "This transaction is no longer active in the current context."
            )
        return stack

    def _unwind(self, commit):
        """Pop one nesting level and finalise when the outermost level closes.

        Shared by the context manager protocol and
        ``FileSystem.end_transaction`` so both apply the same ownership and
        cache-invalidation rules. The stack entry is removed only when the
        outermost level exits, so re-entrant inner exits leave the active
        transaction visible for the still-open outer block.
        """
        stack = self._require_own_stack()
        if stack[-1] is not self:
            raise RuntimeError(
                "Cannot end a transaction that is not the active one in this "
                "context; close the nested transaction first."
            )
        self._depth -= 1
        if self._depth > 0:
            # an inner, re-entrant exit: leave the stack and files for the
            # still-open outer transaction
            return
        # complete() pops the stack and runs _finalize() itself, including on
        # failure, so a raised commit never strands the context mid-transaction
        self.complete(commit=commit)

    def _finalize(self):
        """Tear down per-context bookkeeping after the outermost exit"""
        stacks = _transaction_stacks()
        if not stacks.get(self.fs):
            stacks.pop(self.fs, None)
        fs = self.fs
        if fs is None:
            # already finalised (e.g. an explicit complete followed by an exit)
            return
        # Replay deferred cache invalidations now that writes are visible.
        # The stack has already been popped, so these calls no longer get
        # deferred again by FileSystem.invalidate_cache.
        invalidated = self.invalidated_caches
        self.invalidated_caches = []
        for path in invalidated:
            fs.invalidate_cache(path)
        fs._invalidated_caches_in_transaction = []
        self.fs = None  # break the filesystem -> transaction reference cycle

    def _begin_complete(self):
        """Guard shared by every ``complete`` implementation.

        Raises if the call comes from the wrong context, and makes a second
        completion a no-op. Returns ``False`` when nothing is left to do.
        """
        self._require_owner()
        if self._completed:
            return False
        self._completed = True
        return True

    def complete(self, commit=True):
        """Finish transaction: commit or discard only this transaction's files

        Each deferred file belongs to this transaction, so a failure here does
        not touch files deferred by any other transaction on the same
        filesystem. On failure, the remaining files of *this* transaction are
        still discarded so no temporary files are leaked, and the transaction
        is detached from the current context so later writes are not trapped.
        """
        if not self._begin_complete():
            return
        try:
            if commit:
                self._commit_files()
            else:
                self._discard_files()
        except BaseException:
            # A failed commit must still clean this transaction's own files
            # up, and must never touch another transaction's files.
            self._cleanup_after_failure()
            self._detach_from_context()
            raise
        self._detach_from_context()

    def _detach_from_context(self):
        """Remove this transaction from its context's active stack.

        ``complete`` finalises the whole transaction regardless of nesting
        depth, so any still-open re-entrant levels are closed here too.
        """
        stack = _transaction_stacks().get(self.fs)
        if stack and stack[-1] is self:
            stack.pop()
            self._depth = 0
            self._finalize()

    def _cleanup_after_failure(self):
        """Best-effort discard of whatever this transaction still holds.

        For the default per-file queue, the file that raised is already off
        the queue, so re-queue it so its temporary file is removed too.
        """
        if not isinstance(self.files, deque):
            # non-deque stores (e.g. the dask actor) manage their own cleanup
            return
        while self.files:
            try:
                self._discard_file(self.files.popleft())
            except Exception:
                logger.debug("Discarding deferred file failed", exc_info=True)

    def _commit_files(self):
        """Commit every deferred file of this transaction, one at a time"""
        while self.files:
            f = self.files.popleft()
            try:
                self._commit_file(f)
            except BaseException:
                # put the file that raised back so the cleanup pass discards
                # its temporary output as well
                self.files.appendleft(f)
                raise

    def _discard_files(self):
        """Discard every deferred file of this transaction, one at a time.

        A file whose discard raises stays off the queue: its temporary output
        could not be removed, and retrying it in the failure-cleanup pass would
        only raise again. The remaining files are still best-effort discarded
        by ``complete``.
        """
        while self.files:
            f = self.files.popleft()
            self._discard_file(f)

    def _commit_file(self, f):
        f.commit()

    def _discard_file(self, f):
        f.discard()


class FileActor:
    def __init__(self):
        self.files = []

    def commit(self):
        for f in self.files:
            f.commit()
        self.files.clear()

    def discard(self):
        for f in self.files:
            f.discard()
        self.files.clear()

    def append(self, f):
        self.files.append(f)


class DaskTransaction(Transaction):
    def __init__(self, fs):
        """
        Parameters
        ----------
        fs: FileSystem instance
        """
        super().__init__(fs)
        # the distributed actor replaces the local deque; it is created lazily
        # on start() so merely accessing ``fs.transaction`` has no side effects
        self._client = None

    def start(self):
        import distributed

        self._client = distributed.default_client()
        self.files = self._client.submit(FileActor, actor=True).result()
        super().start()

    def _commit_files(self):
        # the actor holds every deferred file, so commit them in one remote call
        self.files.commit().result()

    def _discard_files(self):
        self.files.discard().result()
