from __future__ import annotations

__lazy_modules__ = [
    "collections",
    "shutil",
    "virtualenv.config.audit",
    "virtualenv.config.convert",
    "virtualenv.config.env_var",
    "virtualenv.config.ini",
]

import os
import shutil
from argparse import SUPPRESS, Action, ArgumentDefaultsHelpFormatter, ArgumentParser, Namespace
from collections import OrderedDict
from typing import TYPE_CHECKING, Any, cast

if TYPE_CHECKING:
    from collections.abc import Iterable, Mapping, Sequence

    from virtualenv.config.audit import ConfigAudit
    from virtualenv.config.convert import TypeData

from virtualenv.config.audit import ActionFacts, Configured, LayerReading, action_kind, build_audit
from virtualenv.config.convert import get_type
from virtualenv.config.env_var import read_env_var
from virtualenv.config.ini import IniConfig


class ExtendAction(Action):
    """Accumulate CLI groups without including configured defaults."""

    def __call__(
        self,
        parser: ArgumentParser,  # ruff:ignore[unused-method-argument] - required argparse signature
        namespace: Namespace,
        values: Sequence[object] | None,
        option_string: str | None = None,  # ruff:ignore[unused-method-argument] - required argparse signature
    ) -> None:
        options = cast("VirtualEnvOptions", namespace)
        previous = getattr(options, self.dest) if options.get_source(self.dest) == "cli" else []
        setattr(options, self.dest, [*previous, *cast("Sequence[object]", values)])


class VirtualEnvConfigParser(ArgumentParser):
    """Custom option parser which updates its defaults by checking the configuration files and environmental vars.

    :param options: the options to populate, a fresh set of defaults when not given
    :param env: the environment variables to read, the process environment when not given
    :param args: positional arguments passed on to :class:`argparse.ArgumentParser`
    :param kwargs: keyword arguments passed on to :class:`argparse.ArgumentParser`

    :var file_config: the configuration file reader
    :var epilog_list: extra epilog entries, empty unless something appends to it

    """

    def __init__(
        self,
        options: VirtualEnvOptions | None = None,
        env: Mapping[str, str] | None = None,
        *args: Any,  # ruff:ignore[any-type]  # keeps ArgumentParser's open-ended constructor arguments
        **kwargs: Any,  # ruff:ignore[any-type]  # keeps ArgumentParser's open-ended constructor arguments
    ) -> None:
        env = os.environ if env is None else env
        self.file_config = IniConfig(env)
        self.epilog_list = []
        self.env = env
        kwargs["epilog"] = self.file_config.epilog
        kwargs["add_help"] = False
        kwargs["formatter_class"] = HelpFormatter
        kwargs["prog"] = "virtualenv"
        super().__init__(*args, **kwargs)
        self._fixed = set()
        if options is not None and not isinstance(options, VirtualEnvOptions):
            msg = "options must be of type VirtualEnvOptions"
            raise TypeError(msg)
        self.options = VirtualEnvOptions() if options is None else options
        self._interpreter = None
        self._app_data = None
        self._audit: dict[int, ActionFacts] = {}
        self._audit_order: list[int] = []
        self._cli_dests: set[str] = set()

    def enable_help(self) -> None:
        """Add the ``-h`` and ``--help`` options, after fixing the defaults."""
        self._fix_defaults()
        self.add_argument("-h", "--help", action="help", default=SUPPRESS, help="show this help message and exit")

    def audit(self) -> ConfigAudit:
        """Report how the configuration layers resolved every option.

        :returns: the audit, built from the same options the creation reads

        """
        return build_audit(self, self.options)

    def _fix_defaults(self) -> None:
        for action in self._actions:
            if (action_id := id(action)) not in self._fixed:
                self._fix_default(action)
                self._fixed.add(action_id)
            elif self.options.get_source(action.dest) == "cli":
                # Each parsing pass starts from the configured default, not the previous CLI values, otherwise
                # accumulating actions (count, append, extend) add the same arguments again on every pass.
                self.options.set_src(action.dest, action.default, getattr(action, "default_source", "default"))

    def _fix_default(self, action: Action) -> None:
        if hasattr(action, "default") and hasattr(action, "dest") and action.default != SUPPRESS:
            as_type = get_type(action)
            names = OrderedDict((i.lstrip("-").replace("-", "_"), None) for i in action.option_strings)
            configured, readings, notes = self._configured_outcome(names, as_type)
            tri_state = action.default is None and isinstance(action.const, bool)
            if tri_state:
                configured, notes = _as_passed_flag(action.const, configured, notes)
            applied = None if configured is None else configured.applied
            # A sibling flag's environment value takes precedence over this flag's configuration file value.
            if configured is not None and not (
                tri_state and applied == "file" and self.options.get_source(action.dest) == "env var"
            ):
                action.default = configured.value
                # argparse stores the source text on the action for the help formatter to show
                vars(action)["default_source"] = configured.display  # simplint: ignore[vars-call]  # argparse metadata  # fmt: skip
                self.options.set_src(action.dest, configured.value, configured.display)
                self._record(action, readings, notes, applied, configured.winner)
            elif tri_state and hasattr(self.options, action.dest):
                # a sibling flag sharing this destination already took a value, so this one adds none; the note comes
                # from whoever did take the value, so nothing is recorded here
                self._record(action, readings, notes, None, None)
            else:
                self.options.set_src(action.dest, action.default, getattr(action, "default_source", "default"))
                self._record(action, readings, notes, None, None)

    def _record(
        self,
        action: Action,
        readings: list[LayerReading],
        notes: list[str],
        applied_source: str | None,
        winner: str | None,
    ) -> None:
        action_id = id(action)
        if action_id not in self._audit:
            self._audit_order.append(action_id)
        self._audit[action_id] = (action, tuple(readings), tuple(notes), applied_source, winner)

    def audit_facts(self) -> list[ActionFacts]:
        """Return what the parser observed while resolving each option, in declaration order.

        :returns: the observed facts of every option the parser resolved

        """
        return [
            ActionFacts(
                action.dest,
                tuple(action.option_strings),
                action_kind(action),
                readings,
                notes,
                applied_source,
                winner,
            )
            for action, readings, notes, applied_source, winner in (
                self._audit[action_id] for action_id in self._audit_order
            )
        ]

    def _configured_outcome(
        self, names: Iterable[str], as_type: TypeData
    ) -> tuple[Configured | None, list[LayerReading], list[str]]:
        readings: list[LayerReading] = []
        notes: list[str] = []
        for name in names:
            env = read_env_var(name, as_type, self.env)
            readings.append(LayerReading("env var", env.name, env.status, env.raw, env.value, env.problem))
        # the config file is only consulted when no env var supplied a value, so report it as skipped when it was not
        consulted = bool(self.file_config)
        for name in names:
            if not consulted:
                readings.append(LayerReading("ini", name, "skipped", None, None, None))
                continue
            ini = self.file_config.read(name, as_type)
            readings.append(
                LayerReading("ini", ini.name, ini.status, ini.raw, ini.value, ini.problem, legacy=ini.legacy)
            )
            if ini.legacy:
                notes.append(f"the config file key {ini.name} uses the historical hyphenated spelling")
        for reading in readings:
            if reading.status == "invalid":
                where = "env var" if reading.layer == "env var" else "config file"
                notes.append(f"{reading.name} at the {where} level cannot be parsed, so it reads as unset")
        for layer in ("env var", "ini"):
            set_values = [i for i in readings if i.layer == layer and i.status == "set"]
            if len(set_values) > 1:
                where = "env var" if layer == "env var" else "config file"
                others = ", ".join(i.name for i in set_values[1:])
                notes.append(f"{set_values[0].name} wins over {others} at the {where} level")
        for reading in readings:
            if reading.status == "set":
                applied = "env var" if reading.layer == "env var" else "file"
                display = f"env var {reading.name}" if applied == "env var" else "file"
                return Configured(reading.value, applied, display, reading.name), readings, notes
        return None, readings, notes

    # narrows the base parse_known_args to VirtualEnvOptions, which ty flags as an override
    def parse_known_args(  # ty: ignore[incompatible-method-override]
        self, args: Sequence[str] | None = None, namespace: VirtualEnvOptions | None = None
    ) -> tuple[VirtualEnvOptions, list[str]]:
        """Parse the arguments, filling the options with values from the config file and the environment.

        :param args: the arguments to parse, ``None`` for the command line arguments
        :param namespace: the options to fill, ``None`` for a new set

        :returns: the parsed options and the arguments that were not recognized

        :raises ValueError: when a value from the config file or the environment cannot be converted

        """
        if namespace is None:
            namespace = self.options
        elif namespace is not self.options:
            msg = "can only pass in parser.options"
            raise ValueError(msg)
        self._fix_defaults()
        # the parser records the source of each option on the options object
        self.options._src = "cli"  # ruff:ignore[private-member-access]
        try:
            namespace.env = self.env
            return super().parse_known_args(args, namespace=namespace)
        finally:
            # the parser records the source of each option on the options object
            self.options._src = None  # ruff:ignore[private-member-access]

    # narrows the base hook to the namespace this parser uses, ty flags it as an override
    def _get_values(  # ty: ignore[incompatible-method-override]
        self, action: Action, arg_strings: list[str]
    ) -> Any:  # ruff:ignore[any-type]  # argparse converts values to the type of the action
        """Record that an option appeared on the command line, then convert its values.

        argparse re-assigns a string default while filling the namespace, which on its own makes a configured default
        look as if it came from the command line. This hook runs only for the options argparse actually acts on, so
        recording the destination here keeps that apart and lets the audit name the real source.

        :param action: the action being applied
        :param arg_strings: the raw values given for it

        :returns: the converted values

        """
        self._cli_dests.add(action.dest)
        return super()._get_values(action, arg_strings)

    def cli_dests(self) -> frozenset[str]:
        """Return the destinations the command line set, as opposed to the configured defaults.

        :returns: the destinations seen on the command line

        """
        return frozenset(self._cli_dests)


class HelpFormatter(ArgumentDefaultsHelpFormatter):
    """Help formatter that shows the defaults, sized to the terminal width.

    :param prog: the program name
    :param kwargs: the keyword arguments passed through to the argparse formatter

    """

    # argparse passes its formatter keywords through untyped
    def __init__(self, prog: str, **kwargs: Any) -> None:  # ruff:ignore[any-type]
        super().__init__(prog, max_help_position=32, width=shutil.get_terminal_size().columns, **kwargs)

    def _get_help_string(self, action: Action) -> str | None:
        text = super()._get_help_string(action)
        if text is not None and hasattr(action, "default_source"):
            default = " (default: %(default)s)"
            if text.endswith(default):
                text = f"{text[: -len(default)]} (default: %(default)s -> from %(default_source)s)"
        return text


class VirtualEnvOptions(Namespace):
    """Namespace of the parsed options, with the source each value came from.

    :param kwargs: the option values

    """

    # Namespace takes arbitrary attributes, so the options arrive as kwargs
    def __init__(self, **kwargs: Any) -> None:  # ruff:ignore[any-type]
        super().__init__(**kwargs)
        self._src: str | None = None
        self._sources: dict[str, str] = {}

    # option values have any type, Namespace stores them as-is
    def __setattr__(self, key: str, value: Any) -> None:  # ruff:ignore[any-type]
        if (src := getattr(self, "_src", None)) is not None:
            self._sources[key] = src
        super().__setattr__(key, value)

    def __repr__(self) -> str:
        return (
            f"{type(self).__name__}({', '.join(f'{k}={v}' for k, v in self.__dict__.items() if not k.startswith('_'))})"
        )

    @property
    def verbosity(self) -> int | None:
        """The verbosity level, computed as ``verbose - quiet``, clamped to zero.

        The value is ``None`` until either ``--verbose`` or ``--quiet`` has been parsed.

        """
        if not hasattr(self, "verbose") and not hasattr(self, "quiet"):
            return None
        return max(self.verbose - self.quiet, 0)

    # option values have any type, Namespace stores them as-is
    def set_src(self, key: str, value: Any, src: str) -> None:  # ruff:ignore[any-type]
        """Set an option value and record where it came from.

        :param key: the option name
        :param value: the option value
        :param src: the source of the value (e.g. ``"cli"``, ``"env var"``, ``"default"``)

        """
        setattr(self, key, value)
        if src.startswith("env var"):
            src = "env var"
        self._sources[key] = src

    def get_source(self, key: str) -> str | None:
        """Return the source that provided a given option value.

        :param key: the option name

        :returns: the source string (e.g. ``"cli"``, ``"env var"``, ``"default"``), or ``None`` if not tracked

        """
        return self._sources.get(key)


def _as_passed_flag(
    const: bool, configured: Configured | None, notes: list[str]
) -> tuple[Configured | None, list[str]]:
    # an env var or ini value for a --x/--no-x pair means "as if the flag was passed": true stores the flag's value,
    # while false on the negative flag leaves the choice unset rather than flipping it on
    if configured is None or const:
        return configured, notes
    if configured.value:
        return configured._replace(value=False), notes
    layer = "env var" if configured.applied == "env var" else "config file"
    notes = [
        *notes,
        f"{configured.winner} at the {layer} level is false, which leaves this flag unset instead of turning on "
        "the opposite flag",
    ]
    return None, notes


__all__ = [
    "ExtendAction",
    "HelpFormatter",
    "VirtualEnvConfigParser",
    "VirtualEnvOptions",
]
