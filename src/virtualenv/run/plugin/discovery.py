from __future__ import annotations

from typing import TYPE_CHECKING

from .base import PluginChoices, PluginLoader

if TYPE_CHECKING:
    from virtualenv.config.cli.parser import VirtualEnvConfigParser
    from virtualenv.discovery.discover import Discover


def get_discover(parser: VirtualEnvConfigParser, args: list[str] | None) -> Discover:
    """Return the discovery of the interpreter, chosen from the installed discovery plugins.

    :param parser: the parser to add the discovery options to
    :param args: the command line arguments

    :returns: the discovery of the interpreter

    :raises RuntimeError: when no discovery plugin is installed

    """
    discover_types = Discovery.entry_points_for("virtualenv.discovery")
    discovery_parser = parser.add_argument_group(
        title="discovery",
        description="discover and provide a target interpreter",
    )
    usable = _get_default_discovery(discover_types)
    # prefer the builtin if present, otherwise fallback to first defined type
    usable = sorted(usable, key=lambda name: 0 if name == "builtin" else 1)
    load_errors = Discovery.load_errors_for("virtualenv.discovery")
    try:
        default_discovery = next(iter(usable))
    except StopIteration as error:
        # a discovery plugin that is installed but failed to load is the reason no interpreter can be found
        if load_errors:
            raise RuntimeError("\n".join(str(failure) for failure in load_errors.values())) from error
        msg = "No discovery plugin found. Try reinstalling virtualenv to fix this issue."
        raise RuntimeError(msg) from error
    # the plugins that failed to load stay selectable, so naming one reports its own error rather than an invalid choice
    choices = PluginChoices(usable, list(load_errors))
    discovery_parser.add_argument(
        "--discovery",
        choices=choices,
        default=default_discovery,
        required=False,
        help="interpreter discovery method",
    )
    options, _ = parser.parse_known_args(args)
    discovery = options.discovery
    if discovery not in discover_types:
        if (load_error := load_errors.get(discovery)) is not None:
            # the plugin is installed but a dependency is missing, which is a different layer than a typo
            raise RuntimeError(str(load_error)) from load_error.error
        available = ", ".join(sorted(discover_types))
        msg = (
            f"discovery {discovery!r} is not available. "
            f"Available discovery methods: {available}. "
            f"Is the plugin installed?"
        )
        raise RuntimeError(msg)
    discover_class = discover_types[discovery]
    # plugin classes come from entry points, which ty sees as type
    discover_class.add_parser_arguments(discovery_parser)  # ty: ignore[missing-attribute]
    options, _ = parser.parse_known_args(args, namespace=options)
    return discover_class(options)


class Discovery(PluginLoader):
    """Discovery plugins."""


def _get_default_discovery(discover_types: dict[str, type]) -> list[str]:
    return list(discover_types.keys())


__all__ = [
    "Discovery",
    "get_discover",
]
