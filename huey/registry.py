from collections import namedtuple

from huey.exceptions import HueyException
from huey.utils import ChordConfig


# Version stamped onto every freshly-produced message. Bump this when the
# on-the-wire message format or task parameters change in a way that requires a
# migration hook to read older messages.
CURRENT_TASK_VERSION = 1

# Messages written before versioning existed carry no version at all. They are
# treated as belonging to this legacy version so that migration hooks can bring
# them up to the current version along an explicitly declared path.
LEGACY_TASK_VERSION = 0


Message = namedtuple('Message', ('id', 'name', 'eta', 'retries', 'retry_delay',
                                 'priority', 'args', 'kwargs', 'on_complete',
                                 'on_error', 'expires', 'expires_resolved',
                                 'timeout', 'chord_config', 'retry_backoff',
                                 'version'))

# Automatically set missing parameters to None. This is kind-of a hack, but it
# allows us to add new parameters while continuing to be able to handle
# messages enqueued with a smaller-set of arguments.
Message.__new__.__defaults__ = (None,) * len(Message._fields)


# Record kept when a message pulled from the broker cannot be read. The raw
# bytes are preserved verbatim -- they are never re-serialized -- so the
# original evidence remains available for diagnostics.
DeadLetter = namedtuple('DeadLetter',
                        ('id', 'raw', 'message', 'error', 'timestamp'))


class Registry(object):
    def __init__(self):
        self._registry = {}
        self._periodic_tasks = []
        # Maps a legacy task name (e.g. old_module.old_function) directly onto
        # the canonical name of a registered task. Aliases always point at a
        # real task, so they can never form a cycle or shadow another alias.
        self._aliases = {}
        # task name -> {(from_version, to_version): migration callable}
        self._migrations = {}
        # Dead-letter records for messages that could not be upgraded/read.
        self._dead_letters = []
        self._dead_letter_count = 0

    def task_to_string(self, task_class):
        return '%s.%s' % (task_class.__module__, task_class.__name__)

    def _coerce_name(self, task):
        # Resolve a TaskWrapper, task class or task-name string to the task's
        # canonical name. No module is imported as part of this lookup.
        if isinstance(task, str):
            return task
        if hasattr(task, 'task_class'):
            task = task.task_class
        return self.task_to_string(task)

    def register(self, task_class):
        task_str = self.task_to_string(task_class)
        if task_str in self._registry:
            raise ValueError('Attempting to register a task with the same '
                             'identifier as existing task. Specify a different'
                             ' name= to register this task. "%s"' % task_str)
        if task_str in self._aliases:
            raise ValueError('Cannot register task "%s": this name is already '
                             'declared as an alias for "%s".' %
                             (task_str, self._aliases[task_str]))

        self._registry[task_str] = task_class
        if hasattr(task_class, 'validate_datetime'):
            self._periodic_tasks.append(task_class)
        return True

    def unregister(self, task_class):
        task_str = self.task_to_string(task_class)
        if task_str not in self._registry:
            return False

        del self._registry[task_str]
        # Any aliases and migrations pointing at the removed task are no longer
        # resolvable, so drop them as well.
        for alias in [name for name, target in self._aliases.items()
                      if target == task_str]:
            del self._aliases[alias]
        self._migrations.pop(task_str, None)
        if hasattr(task_class, 'validate_datetime'):
            self._periodic_tasks = [t for t in self._periodic_tasks
                                    if t is not task_class]
        return True

    def add_alias(self, task, alias):
        """
        Register a legacy ``alias`` name for ``task``. Messages that carry the
        alias name are resolved to the registered task before they are built.

        Aliases must point at a currently-registered task and cannot reuse the
        name of a real task or of another alias, which guarantees the alias
        graph can never contain a cycle or shadow a real task.
        """
        target = self._coerce_name(task)
        if target not in self._registry:
            raise HueyException('Cannot add alias "%s": target task "%s" is '
                                'not registered.' % (alias, target))
        if alias in self._registry:
            raise ValueError('Cannot declare alias "%s": a real task with '
                             'that name is already registered.' % alias)
        existing = self._aliases.get(alias)
        if existing is not None and existing != target:
            raise ValueError('Alias "%s" is already declared for task "%s".' %
                             (alias, existing))
        self._aliases[alias] = target
        return target

    def remove_alias(self, alias):
        return self._aliases.pop(alias, None) is not None

    def resolve_name(self, name):
        """
        Resolve a message's task name, following aliases, and return the
        canonical name. Raises ``HueyException`` if no registered task matches
        or if an alias loop is encountered.
        """
        seen = set()
        current = name
        while current in self._aliases:
            if current in seen:
                raise HueyException('Alias cycle detected while resolving '
                                    'task name "%s".' % name)
            seen.add(current)
            current = self._aliases[current]
        if current not in self._registry:
            raise HueyException('%s not found in TaskRegistry' % name)
        return current

    def add_message_migration(self, task, from_version, to_version, migrator):
        """
        Declare a single version migration step for ``task``.

        ``migrator`` receives a ``Message`` and returns a ``Message`` whose
        parameters are suitable for ``to_version``. Migrations may only be
        traversed along declared edges, and the declared edges must not form a
        cycle.
        """
        if not callable(migrator):
            raise ValueError('Message migration must be callable.')
        if from_version == to_version:
            raise ValueError('Cannot declare a migration between identical '
                             'versions (%s).' % from_version)
        name = self._coerce_name(task)
        if name not in self._registry:
            raise HueyException('Cannot register migration: task "%s" is not '
                                'registered.' % name)

        edges = self._migrations.setdefault(name, {})
        edge = (from_version, to_version)
        if edge in edges and edges[edge] is not migrator:
            raise ValueError('Migration %s -> %s for task "%s" is already '
                             'declared.' % (from_version, to_version, name))
        edges[edge] = migrator

        # A cycle would let an upgrade loop forever, so reject the edge.
        if self._migration_edges_have_cycle(edges):
            del edges[edge]
            raise ValueError('Migration for task "%s" would introduce a '
                             'version cycle.' % name)
        return True

    def _migration_edges_have_cycle(self, edges):
        # Build an adjacency map of declared edges and look for a cycle with
        # an iterative depth-first search.
        adjacency = {}
        for source, destination in edges:
            adjacency.setdefault(source, set()).add(destination)

        visited = set()
        for start in adjacency:
            if start in visited:
                continue
            stack = [(start, False)]
            path = set()
            while stack:
                node, leaving = stack.pop()
                if leaving:
                    path.discard(node)
                    visited.add(node)
                    continue
                if node in path:
                    return True
                if node in visited:
                    continue
                path.add(node)
                stack.append((node, True))
                for neighbor in adjacency.get(node, ()):
                    if neighbor in path:
                        return True
                    if neighbor not in visited:
                        stack.append((neighbor, False))
        return False

    def _migration_path(self, name, from_version, to_version):
        """
        Return the ordered list of (from, to, migrator) edges that upgrade
        ``name`` from ``from_version`` to ``to_version``. Only explicitly
        declared edges may be used.
        """
        edges = self._migrations.get(name, {})
        adjacency = {}
        for source, destination in edges:
            adjacency.setdefault(source, set()).add(destination)

        queue = [(from_version, [])]
        seen = {from_version}
        while queue:
            version, path = queue.pop(0)
            if version == to_version:
                return [(source, destination, edges[(source, destination)])
                        for source, destination in path]
            for neighbor in adjacency.get(version, ()):
                if neighbor not in seen:
                    seen.add(neighbor)
                    queue.append((neighbor, path + [(version, neighbor)]))
        raise HueyException('No declared migration path for task "%s" from '
                            'version %s to %s.' %
                            (name, from_version, to_version))

    def current_version(self, task_class):
        return getattr(task_class, 'message_version', CURRENT_TASK_VERSION)

    def create_message(self, task):
        task_str = self.task_to_string(type(task))
        if task_str not in self._registry:
            raise HueyException('%s not found in TaskRegistry' % task_str)

        # Remove an injected context-task instance before serializing.
        if task.kwargs and task.kwargs.get('task') is task:
            task.kwargs.pop('task')

        on_complete = None
        if task.on_complete is not None:
            on_complete = self.create_message(task.on_complete)

        on_error = None
        if task.on_error is not None:
            on_error = self.create_message(task.on_error)

        chord_config = None
        if task.chord_config is not None:
            chord_config = (
                task.chord_config.cid,
                task.chord_config.size,
                task.chord_config.idx,
                self.create_message(task.chord_config.callback))

        # New messages are always written with the task's current name and
        # version -- aliases and legacy versions are only used when reading.
        return Message(
            task.id,
            task_str,
            task.eta,
            task.retries,
            task.retry_delay,
            task.priority,
            task.args,
            task.kwargs,
            on_complete,
            on_error,
            task.expires,
            task.expires_resolved,
            task.timeout,
            chord_config,
            task.retry_backoff,
            self.current_version(type(task)))

    def upgrade_message(self, message):
        """
        Resolve a (possibly legacy) message to the current task name and
        upgrade its parameters along the declared migration path. Nested
        pipeline and chord callback messages are upgraded recursively.
        """
        canonical = self.resolve_name(message.name)

        version = message.version
        if version is None:
            # The message predates message versioning.
            version = LEGACY_TASK_VERSION

        current = self.current_version(self._registry[canonical])
        upgraded = message
        if version != current:
            if version > current:
                raise HueyException('Message for task "%s" has version %s, '
                                    'which is newer than the supported '
                                    'version %s.' %
                                    (message.name, version, current))
            path = self._migration_path(canonical, version, current)
            for from_version, to_version, migrator in path:
                upgraded = migrator(upgraded)
                # The migration hook only transforms parameters; the registry
                # is responsible for bookkeeping (canonical name/version).
                upgraded = upgraded._replace(name=canonical,
                                             version=to_version)
        elif upgraded.name != canonical:
            upgraded = upgraded._replace(name=canonical)

        if upgraded.on_complete is not None:
            upgraded = upgraded._replace(
                on_complete=self.upgrade_message(upgraded.on_complete))
        if upgraded.on_error is not None:
            upgraded = upgraded._replace(
                on_error=self.upgrade_message(upgraded.on_error))
        if upgraded.chord_config is not None:
            cid, size, idx, callback = upgraded.chord_config
            upgraded = upgraded._replace(chord_config=(
                cid, size, idx, self.upgrade_message(callback)))
        return upgraded

    def create_task(self, message):
        # First identify the target task (resolving any legacy alias), then
        # upgrade the parameters to the current version before constructing.
        message = self.upgrade_message(message)
        TaskClass = self.string_to_task(message.name)

        on_complete = None
        if message.on_complete is not None:
            on_complete = self.create_task(message.on_complete)

        on_error = None
        if message.on_error is not None:
            on_error = self.create_task(message.on_error)

        chord_config = None
        if message.chord_config is not None:
            cid, size, idx, cb_ser = message.chord_config
            callback = self.create_task(cb_ser)
            chord_config = ChordConfig(cid, size, idx, callback)

        return TaskClass(
            message.args,
            message.kwargs,
            message.id,
            message.eta,
            message.retries,
            message.retry_delay,
            message.priority,
            message.expires,
            on_complete,
            on_error,
            message.expires_resolved,
            message.timeout,
            chord_config,
            message.retry_backoff)

    def string_to_task(self, task_str):
        canonical = self.resolve_name(task_str)
        return self._registry[canonical]

    def add_dead_letter(self, raw, message, error, timestamp):
        """
        Retain a message that could not be read/upgraded. ``raw`` is stored
        exactly as it came off the broker -- it is never re-serialized -- so
        the original evidence is preserved for diagnostics.
        """
        self._dead_letter_count += 1
        record = DeadLetter(self._dead_letter_count, raw, message, error,
                            timestamp)
        self._dead_letters.append(record)
        return record

    @property
    def dead_letters(self):
        return list(self._dead_letters)

    def get_dead_letter(self, id):
        for record in self._dead_letters:
            if record.id == id:
                return record

    def clear_dead_letters(self):
        count = len(self._dead_letters)
        self._dead_letters = []
        return count

    @property
    def periodic_tasks(self):
        return [task_class() for task_class in self._periodic_tasks]
