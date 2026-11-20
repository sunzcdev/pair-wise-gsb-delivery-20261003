from __future__ import annotations

__lazy_modules__ = [
    "collections",
    "importlib.metadata",
]

import logging
import sys
from collections import OrderedDict
from importlib.metadata import entry_points
from typing import TYPE_CHECKING, ClassVar, Final, NamedTuple

if TYPE_CHECKING:
    from collections.abc import Iterator, Sequence
    from typing import NoReturn

    from python_discovery import PythonInfo

    from virtualenv.config.cli.parser import VirtualEnvConfigParser, VirtualEnvOptions

LOGGER: Final[logging.Logger] = logging.getLogger(__name__)  # simplint: ignore[module-variable-prefix]  # existing name


class PluginLoadError(NamedTuple):
    """An entry point that could not be loaded, kept with the error its load raised.

    :param group: the entry point group the plugin registers under
    :param name: the entry point name the plugin registers under
    :param value: the entry point value naming the plugin class
    :param error: the error loading the plugin raised

    """

    group: str
    name: str
    value: str
    error: BaseException

    def __str__(self) -> str:
        return (
            f"{self.group} plugin {self.name!r} ({self.value}) is installed but failed to load: "
            f"{type(self.error).__name__}: {self.error}"
        )


class PluginLoader:
    """Load the plugin classes registered under the entry point groups."""

    _OPTIONS = None
    _ENTRY_POINTS = None
    _LOAD_ERRORS: ClassVar[dict[str, dict[str, PluginLoadError]]] = {}

    @classmethod
    def entry_points_for(cls, key: str) -> OrderedDict[str, type]:
        """Return the plugin classes registered under an entry point group.

        A plugin that fails to load, for example because one of its dependencies is not installed, is left out and
        remembered in :meth:`load_errors_for`, so the plugins that do load stay usable.

        :param key: the entry point group name

        :returns: the plugin classes keyed by their entry point name

        """
        if sys.version_info >= (3, 10):
            # ty on Python 3.10 and 3.15 types entry_points() as object, which has no select
            selected = list(cls.entry_points().select(group=key))  # ty: ignore[missing-attribute]
        else:
            # ty on Python 3.9 types entry_points() as object, which has no get
            selected = list(cls.entry_points().get(key, []))  # ty: ignore[missing-attribute]
        # Third-party packages may register entry points with the same name as virtualenv's
        # built-ins (e.g. xonsh's own `virtualenv.activate.xonsh`). Sort so built-ins are
        # inserted last into the OrderedDict, making them win on name collision.
        selected.sort(key=lambda entry: entry.value.startswith("virtualenv."))
        result: OrderedDict[str, type] = OrderedDict()
        errors: dict[str, PluginLoadError] = {}
        for entry in selected:
            # a plugin missing a dependency must not hide the plugins that do load, so keep the failure aside for when
            # someone selects it explicitly
            try:
                result[entry.name] = entry.load()
            # an import error is what a plugin with a missing dependency raises, and the loop runs a handful of times
            except Exception as exception:  # ruff:ignore[blind-except, try-except-in-loop]
                errors[entry.name] = PluginLoadError(key, entry.name, entry.value, exception)
                LOGGER.warning("%s", errors[entry.name])
            else:
                # a later entry point of the same name loaded, so it wins over the failure recorded before it
                errors.pop(entry.name, None)
        cls._LOAD_ERRORS[key] = errors
        return result

    @classmethod
    def load_errors_for(cls, key: str) -> dict[str, PluginLoadError]:
        """Return the plugins that failed to load the last time an entry point group was read.

        :param key: the entry point group name

        :returns: the load errors keyed by their entry point name

        """
        return cls._LOAD_ERRORS.get(key, {})

    @staticmethod
    def entry_points() -> object:  # simplint: ignore[imprecise-annotation]  # importlib.metadata type varies by Python
        """Return the entry points of all installed packages, loaded once per process.

        :returns: the entry points

        """
        if PluginLoader._ENTRY_POINTS is None:
            PluginLoader._ENTRY_POINTS = entry_points()
        return PluginLoader._ENTRY_POINTS


class PluginChoices:
    """The choices of a plugin selection option: the usable plugins, plus the registered ones that cannot run here.

    Iterating yields only the usable plugin names, which is what ``--help`` and the invalid choice message show.
    Membership also accepts the unusable names, so selecting one reports why it failed instead of being rejected as an
    unknown name.

    :param usable: the plugin names that can run here
    :param unavailable: the plugin names that are registered but cannot run here

    """

    def __init__(self, usable: Sequence[str], unavailable: Sequence[str]) -> None:
        self.usable = list(usable)
        self.unavailable = [name for name in unavailable if name not in self.usable]

    def __iter__(self) -> Iterator[str]:
        return iter(self.usable)

    def __len__(self) -> int:
        return len(self.usable)

    def __contains__(self, item: object) -> bool:
        return item in self.usable or item in self.unavailable

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(usable={self.usable}, unavailable={self.unavailable})"


class ComponentBuilder(PluginLoader):
    """Build the plugin choice for one component type, with the options of the chosen plugin.

    :param interpreter: the interpreter the environment is created from
    :param parser: the parser to add the options to
    :param name: the component type name, used as the option name
    :param possible: the plugin classes keyed by their name
    :param unavailable: the plugins that loaded but cannot serve the interpreter, with the reason why

    :var name: the component type name
    :var possible: the plugin classes keyed by their name
    :var load_errors: the plugins the entry point group could not load, keyed by their name
    :var unavailable: every plugin that cannot be selected, with the reason why, keyed by its name

    """

    #: The entry point group the plugins of this component register under, ``None`` when the caller loads them.
    entry_point_group: ClassVar[str | None] = None

    def __init__(
        self,
        interpreter: PythonInfo,
        parser: VirtualEnvConfigParser,
        name: str,
        possible: dict[str, type],
        unavailable: dict[str, str] | None = None,
    ) -> None:
        self.interpreter = interpreter
        self.name = name
        self._impl_class = None
        self.possible = possible
        self.load_errors = {} if self.entry_point_group is None else self.load_errors_for(self.entry_point_group)
        # a load failure already says why the plugin is unusable, so it wins over a capability reason
        self.unavailable = {
            **(unavailable or {}),
            **{key: str(error) for key, error in self.load_errors.items() if key not in possible},
        }
        self.parser = parser.add_argument_group(title=name)
        self.add_selector_arg_parse(name, list(self.possible))

    @property
    def choices(self) -> PluginChoices:
        """The choices of the selection option, which help shows as the usable plugin names."""
        return PluginChoices(self.possible, list(self.unavailable))

    def add_selector_arg_parse(self, name: str, choices: Sequence[str]) -> None:
        """Add the option that selects the plugin, to be implemented by subclasses.

        :param name: the option name
        :param choices: the plugin names to choose from

        :raises NotImplementedError: always, subclasses implement it

        """
        raise NotImplementedError

    @classmethod
    def options(cls, key: str) -> OrderedDict[str, type]:
        """Return the options of the plugin type, loaded once per process.

        :param key: the entry point group name

        :returns: the plugin classes keyed by their name

        """
        if cls._OPTIONS is None:
            cls._OPTIONS = cls.entry_points_for(key)
        return cls._OPTIONS

    def handle_selected_arg_parse(self, options: VirtualEnvOptions) -> str:
        """Validate the selected plugin name, load its options and return it.

        :param options: the parsed options

        :returns: the selected plugin name

        :raises RuntimeError: when the selected plugin is not installed, failed to load, or cannot serve the interpreter

        """
        selected = getattr(options, self.name)
        if selected not in self.possible:
            # the name is accepted by the parser so the failure can be reported per plugin, and not as a bare invalid
            # choice - argparse checks choices only on the command line, so a value from an env var or ini file lands
            # here too
            self._raise_unavailable(selected)
        self._impl_class = self.possible[selected]
        self.populate_selected_argparse(selected, options.app_data)
        return selected

    def _raise_unavailable(self, selected: str) -> NoReturn:
        load_error = self.load_errors.get(selected)
        if load_error is not None:
            # the plugin was registered but its import failed, which is a load layer failure, so report it as such and
            # keep the original error as the cause rather than silently picking another implementation
            msg: Final[str] = str(load_error)
            raise RuntimeError(msg) from load_error.error
        reason = self.unavailable.get(selected)
        if reason is not None:
            # the plugin did load, it just cannot serve this interpreter - a capability layer failure, not a load one
            msg = f"{self.name} {selected!r} cannot be used{self._available_for()}: {reason}"
            raise RuntimeError(msg)
        msg = (
            f"{self.name} {selected!r} is not available{self._available_for()}. "
            f"Available {self.name}s: {', '.join(sorted(self.possible))}. Is the plugin installed?"
        )
        raise RuntimeError(msg)

    def _available_for(self) -> str:
        return ""

    def populate_selected_argparse(
        self,
        selected: str,
        app_data: object,  # simplint: ignore[imprecise-annotation]  # public signature
    ) -> None:
        """Set the parser description and create the chosen plugin's options.

        :param selected: the selected plugin name
        :param app_data: the application data folder

        """
        self.parser.description = f"options for {self.name} {selected}"
        assert self._impl_class is not None  # ruff:ignore[assert]  # Set by handle_selected_arg_parse
        # plugin classes come from entry points, which ty sees as type
        self._impl_class.add_parser_arguments(self.parser, self.interpreter, app_data)  # ty: ignore[missing-attribute]

    def create(
        self, options: VirtualEnvOptions
    ) -> object:  # simplint: ignore[imprecise-annotation]  # entry point class instance, type known at run time
        """Create the chosen plugin.

        :param options: the parsed options

        :returns: the plugin instance

        """
        assert self._impl_class is not None  # ruff:ignore[assert]  # Set by handle_selected_arg_parse
        return self._impl_class(options, self.interpreter)


__all__ = [
    "ComponentBuilder",
    "PluginChoices",
    "PluginLoadError",
    "PluginLoader",
]
