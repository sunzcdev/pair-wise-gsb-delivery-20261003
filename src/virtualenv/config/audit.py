"""Explain how the configuration layers resolved each option.

Every option can come from the command line, an environment variable or the configuration file. A report built from the
same parse the creation uses shows what each layer offered, which layer the parser took, and why the layers it skipped
were skipped.

"""

from __future__ import annotations

__lazy_modules__ = ["typing"]

from typing import TYPE_CHECKING, NamedTuple

if TYPE_CHECKING:
    from collections.abc import Iterable

    from .cli.parser import VirtualEnvConfigParser, VirtualEnvOptions

#: How each kind of option combines the values of the layers.
KIND_HINTS = {
    "flag": "a boolean flag; a configured true counts as passing it",
    "list": "a list; the command line replaces the configured items, repeating the option adds more",
    "count": "a counter; the command line counts on top of the configured number",
    "value": "a single value; the layer with the highest priority wins",
}
#: The layer names, highest priority first.
LAYERS = ("cli", "env var", "ini", "default")
_FLAG_ACTIONS = frozenset({"_StoreTrueAction", "_StoreFalseAction", "_StoreConstAction"})
_COUNT_ACTIONS = frozenset({"_CountAction"})
_APPEND_ACTIONS = frozenset({"_AppendAction", "_AppendConstAction"})
_AUDIT_SOURCE = {"file": "ini"}


class LayerReading(NamedTuple):
    """What one configuration layer offered for an option, even when the parser did not use it.

    :param layer: ``env var`` or ``ini``
    :param name: the environment variable or configuration key
    :param status: ``set``, ``empty``, ``invalid``, ``unset`` or ``skipped``
    :param raw: the raw value, ``None`` when the layer has none
    :param value: the converted value, ``None`` unless the status is ``set``
    :param problem: why the value cannot be parsed, ``None`` unless the status is ``invalid``
    :param legacy: ``True`` when an ini key used the historical hyphenated spelling
    :param used: ``True`` when the value is the one the option ends up with

    """

    layer: str
    name: str
    status: str
    raw: str | None
    value: object
    problem: str | None
    legacy: bool = False
    used: bool = False


class Configured(NamedTuple):
    """The value a configuration layer supplies for an option, with how to name that layer.

    :param value: the converted value
    :param applied: ``env var`` or ``file``, the source the parser records for the option
    :param display: the source text shown in help and stored as the default source
    :param winner: the environment variable or configuration key that supplied the value

    """

    value: object
    applied: str
    display: str
    winner: str


class ActionFacts(NamedTuple):
    """What the parser observed while resolving the options of one argparse action.

    :param dest: the option destination
    :param option_strings: the command line spellings of the option
    :param kind: ``flag``, ``list``, ``count`` or ``value``
    :param readings: what the configuration file and environment offered
    :param notes: why a layer was skipped or turned into no choice at all
    :param applied_source: the layer that supplied the configured default, ``None`` when a sibling option set it first
    :param winner: the environment variable or key that supplied that default, ``None`` for a hardcoded default

    """

    dest: str
    option_strings: tuple[str, ...]
    kind: str
    readings: tuple[LayerReading, ...]
    notes: tuple[str, ...]
    applied_source: str | None
    winner: str | None


class OptionAudit(NamedTuple):
    """The final value of one option, with the layers that decided it.

    :param dest: the option destination
    :param option_strings: the command line spellings of the option
    :param kind: ``flag``, ``list``, ``count`` or ``value``
    :param value: the value the parsed options hold, which is what the creation reads
    :param source: the layer that provided the value, ``cli``, ``env var``, ``ini`` or ``default``
    :param readings: what the configuration file and environment layers offered
    :param notes: why a layer was skipped, overridden or turned into no choice

    """

    dest: str
    option_strings: tuple[str, ...]
    kind: str
    value: object
    source: str | None
    readings: tuple[LayerReading, ...]
    notes: tuple[str, ...]


class ConfigAudit:
    """The resolved options of one parse, reported layer by layer.

    :param config_file: the path of the configuration file the parser read
    :param config_state: ``active``, ``missing`` or ``failed to parse``
    :param config_from_env_var: whether ``VIRTUALENV_CONFIG_FILE`` named the configuration file
    :param options: the audit of every option the parser resolved, in declaration order

    """

    def __init__(
        self,
        config_file: str,
        config_state: str,
        config_from_env_var: bool,
        options: tuple[OptionAudit, ...],
    ) -> None:
        self.config_file = config_file
        self.config_state = config_state
        self.config_from_env_var = config_from_env_var
        self.options = options

    def get(self, dest: str) -> OptionAudit | None:
        """Return the audit of one option.

        :param dest: the option destination, or its command line spelling such as ``--no-periodic-update``

        :returns: the audit, ``None`` when the parser never resolved that option

        """
        wanted = normalize_dest(dest)
        return next((option for option in self.options if option.dest == wanted), None)

    def value(self, dest: str) -> object:
        """Return the final value of one option, the value the creation reads.

        :param dest: the option destination

        :returns: the value, ``None`` when the parser never resolved that option

        """
        option = self.get(dest)
        return None if option is None else option.value

    def source(self, dest: str) -> str | None:
        """Return the layer that provided the final value of one option.

        :param dest: the option destination

        :returns: ``cli``, ``env var``, ``ini`` or ``default``, and ``None`` when the option was never resolved

        """
        option = self.get(dest)
        return None if option is None else option.source

    def readings(self, dest: str, layer: str | None = None) -> tuple[LayerReading, ...]:
        """Return what the configuration file and environment offered for one option.

        :param dest: the option destination
        :param layer: only keep the readings of this layer, ``None`` for all of them

        :returns: the readings, empty when the option was never resolved

        """
        option = self.get(dest)
        if option is None:
            return ()
        return tuple(i for i in option.readings if layer is None or i.layer == layer)

    def interesting(self) -> tuple[str, ...]:
        """Return the options a configuration layer or the command line set, the ones a report can explain.

        Options that fell back to their built-in default are left out, since nothing decided them, but an option a
        layer tried to set and could not parse still appears, so a broken value is not hidden by the omission.

        :returns: the destinations worth reporting, in declaration order

        """
        return tuple(
            option.dest
            for option in self.options
            if option.source not in {None, "default"}
            or any(reading.status in {"set", "invalid", "empty"} for reading in option.readings)
        )

    def header(self) -> str:
        """Return the line that describes the configuration file."""
        changed = " (named by env var VIRTUALENV_CONFIG_FILE)" if self.config_from_env_var else ""
        return f"config file {self.config_file}: {self.config_state}{changed}"

    def summary(self) -> tuple[str, ...]:
        """Return what the parse hands to the creation, phrased as the creation reads it.

        A negative flag is reported as the behavior it turns off, so a report about ``--no-periodic-update`` says
        whether the periodic update runs rather than leaving the reader to invert a boolean.

        :returns: one line per option the creation consumes, in a fixed order

        """
        lines: list[str] = []
        if (dest := self.get("dest")) is not None:
            lines.append(f"destination: {_format(dest.value)}")
        for key in ("creator", "seeder"):
            if (option := self.get(key)) is not None:
                lines.append(f"{key}: {_format(option.value)} from {option.source}")
        if (option := self.get("activators")) is not None:
            lines.append(f"activators: {_format(option.value)} from {option.source}")
        if (option := self.get("no_periodic_update")) is not None:
            state = "enabled" if not option.value else "disabled"
            lines.append(f"periodic update: {state} (no_periodic_update from {option.source})")
        if (option := self.get("download")) is not None:
            lines.append(f"download: {_format(option.value)} from {option.source}")
        return tuple(lines)

    # the two flags keep printable defaults for callers that only want the blocks
    def render(  # ruff:ignore[boolean-default-value-positional-argument]
        self, dests: Iterable[str] | None = None, include_summary: bool = True
    ) -> str:
        """Render the audit as text, one block per option.

        :param dests: the destinations to report, ``None`` for all of them
        :param include_summary: ``True`` to lead with what the creation reads, phrased in its own terms

        :returns: the report

        """
        wanted = None if dests is None else {normalize_dest(i) for i in dests}
        chosen = tuple(i for i in self.options if wanted is None or i.dest in wanted)
        lines = [self.header()]
        summary = self.summary() if include_summary else ()
        if summary:
            lines.extend(("", "resolved:", *(f"  {line}" for line in summary)))
        for option in chosen:
            lines.extend(("", *_render_option(option)))
        return "\n".join(lines)


def action_kind(action: object) -> str:
    """Classify an argparse action, so the report can explain how its value combines.

    :param action: the argparse action

    :returns: ``flag``, ``list``, ``count`` or ``value``

    """
    name = type(action).__name__
    if name in _FLAG_ACTIONS:
        return "flag"
    if name in _COUNT_ACTIONS:
        return "count"
    nargs = getattr(action, "nargs", None)
    if name in _APPEND_ACTIONS or nargs in {"+", "*"} or isinstance(getattr(action, "default", None), list):
        return "list"
    return "value"


def normalize_dest(value: str) -> str:
    """Turn an option spelling such as ``--no-periodic-update`` into the destination ``no_periodic_update``.

    :param value: the option as a user spelled it, or the destination itself

    :returns: the destination

    """
    return value.strip().lstrip("-").replace("-", "_")


def build_audit(parser: VirtualEnvConfigParser, options: VirtualEnvOptions) -> ConfigAudit:
    """Build the audit of the options a parser resolved.

    The values come from the same options object the creation reads, so the report cannot disagree with it.

    :param parser: the parser that resolved the options
    :param options: the parsed options

    :returns: the audit

    """
    order: list[str] = []
    grouped: dict[str, list[ActionFacts]] = {}
    for fact in parser.audit_facts():
        if fact.dest not in grouped:
            order.append(fact.dest)
            grouped[fact.dest] = []
        grouped[fact.dest].append(fact)
    from_cli = parser.cli_dests()
    entries = tuple(_build_option(dest, grouped[dest], options, dest in from_cli) for dest in order)
    file_config = parser.file_config
    return ConfigAudit(
        str(file_config.config_file),
        file_config.STATE[file_config.has_config_file],
        file_config.is_env_var,
        entries,
    )


def _build_option(dest: str, facts: list[ActionFacts], options: VirtualEnvOptions, on_cli: bool) -> OptionAudit:
    spellings: list[str] = []
    readings: list[LayerReading] = []
    notes: list[str] = []
    for fact in facts:
        spellings.extend(i for i in fact.option_strings if i not in spellings)
        readings.extend(fact.readings)
        notes.extend(i for i in fact.notes if i not in notes)
    kind = facts[0].kind
    # the last sibling that supplied a configured default is the one argparse kept for the shared destination
    applied = next((fact for fact in reversed(facts) if fact.applied_source not in {None, "default"}), None)
    source = _resolve_source(on_cli, applied)
    marked = _mark_used(readings, source, applied, kind)
    notes.extend(_notes_for(source, marked, kind))
    value = getattr(options, dest, None)
    return OptionAudit(dest, tuple(spellings), kind, value, source, marked, tuple(notes))


def _resolve_source(on_cli: bool, applied: ActionFacts | None) -> str:
    if on_cli:
        return "cli"
    # argparse re-assigns string defaults while it fills the namespace, which makes a configured default look as if it
    # came from the command line; only a configured default or the built-in one is left here
    if applied is None:
        return "default"
    return option_source(applied.applied_source) or "default"


def option_source(source: str | None) -> str | None:
    """Name a layer as the report shows it.

    :param source: the source the parser records, such as ``file``

    :returns: the report name, such as ``ini``

    """
    return _AUDIT_SOURCE.get(source, source)


def _mark_used(
    readings: list[LayerReading], source: str | None, applied: ActionFacts | None, kind: str
) -> tuple[LayerReading, ...]:
    # a counter keeps the configured number and adds the command line count, so its configured reading still counts
    if applied is None or source is None or (source not in {"env var", "ini"} and kind != "count"):
        return tuple(readings)
    if source == "cli" and kind != "count":
        return tuple(readings)
    used_layer = "env var" if applied.applied_source == "env var" else "ini"
    result, marked = [], False
    for reading in readings:
        take = not marked and reading.layer == used_layer and reading.name == applied.winner and reading.status == "set"
        marked = marked or take
        result.append(reading._replace(used=take))
    return tuple(result)


def _notes_for(source: str | None, readings: tuple[LayerReading, ...], kind: str) -> list[str]:
    notes: list[str] = []
    if source == "cli":
        if kind == "count":
            # a counter adds the command line count to the configured number rather than replacing it
            notes.extend(
                f"the command line adds to the {i.layer} count {i.raw!r} instead of replacing it"
                for i in readings
                if i.used
            )
        elif any(i.status in {"set", "invalid", "empty"} for i in readings):
            notes.append("the command line overrides the config file and the environment variables")
    elif source == "env var":
        if any(i.layer == "ini" and i.status == "set" for i in readings):
            notes.append("an environment variable outranks the config file")
    return notes


def _render_option(option: OptionAudit) -> list[str]:
    spellings = ", ".join(option.option_strings) or option.dest
    lines = [
        f"{option.dest} ({spellings}): {_format(option.value)} from {option.source or 'unset'}",
        f"  kind: {option.kind} - {KIND_HINTS[option.kind]}",
    ]
    lines.extend(f"  {_render_reading(i)}" for i in option.readings)
    lines.append(f"  cli: {'passed' if option.source == 'cli' else 'not given'}")
    lines.extend(f"  note: {note}" for note in option.notes)
    return lines


def _render_reading(reading: LayerReading) -> str:
    where = "env" if reading.layer == "env var" else "ini"
    if reading.status == "set":
        text = f"{reading.name}={reading.raw!r} -> {_format(reading.value)}"
        text += " (used)" if reading.used else " (not used)"
    elif reading.status == "empty":
        text = f"{reading.name} is set to an empty string, which counts as unset"
    elif reading.status == "invalid":
        text = f"{reading.name}={reading.raw!r} cannot be parsed: {reading.problem}"
    elif reading.status == "skipped":
        text = f"{reading.name} not checked, the config file is not in use"
    else:
        text = f"{reading.name} not set"
    if reading.legacy:
        text += " [historical hyphenated key]"
    return f"{where}: {text}"


def _format(value: object) -> str:
    if isinstance(value, (list, tuple)):
        return "[" + ", ".join(_format(i) for i in value) + "]"
    return repr(value)


__all__ = [
    "ActionFacts",
    "ConfigAudit",
    "Configured",
    "KIND_HINTS",
    "LAYERS",
    "LayerReading",
    "OptionAudit",
    "action_kind",
    "build_audit",
    "normalize_dest",
    "option_source",
]


