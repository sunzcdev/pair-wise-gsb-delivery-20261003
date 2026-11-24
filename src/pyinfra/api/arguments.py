from __future__ import annotations

import os
from typing import (
    TYPE_CHECKING,
    Any,
    Generic,
    TypeVar,
    cast,
    get_type_hints,
)
from collections.abc import Callable, Iterable, Mapping

from typing_extensions import TypedDict, override

from pyinfra.api.exceptions import ArgumentTypeError
from pyinfra.api.util import raise_if_bad_type
from pyinfra.context import ctx_config
from pyinfra.api.hiddenvalue import HiddenValue

if TYPE_CHECKING:
    from pyinfra.api import Config, Host, State

T = TypeVar("T")
default_sentinel = object()


class Unset:
    """
    Sentinel used to explicitly remove an inherited argument in a nested scope.

    This is intentionally distinct from ``None``: ``None`` is a valid value for
    many arguments, while :data:`UNSET` means the argument should not be passed
    to the connector in the current scope.
    """

    __slots__ = ()

    def __repr__(self) -> str:
        return "UNSET"

    def __bool__(self) -> bool:
        return False


UNSET = Unset()
# Common alternative type/sentinel names used by callers and integrations.
UnsetType = Unset
unset = UNSET


def is_unset(value: Any) -> bool:
    """Return whether ``value`` is an explicit unset sentinel."""

    return (
        value is UNSET
        or value is unset
        or value is default_sentinel
        or value is Unset
        or isinstance(value, Unset)
    )


class LayeredDict(dict[str, Any]):
    """
    A dict carrying keys that were explicitly unset while it was resolved.

    The concrete dict values remain safe resolved values, while
    :attr:`unset_keys` lets nested scopes and connector construction preserve
    the distinction between an explicit ``None`` and an explicit unset.
    """

    _unset_keys: set[str]

    def __init__(self, *args: Any, unset_keys: Iterable[str] = (), **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        self._unset_keys = set(unset_keys)

    @property
    def unset_keys(self) -> frozenset[str]:
        return frozenset(self._unset_keys)

    @unset_keys.setter
    def unset_keys(self, keys: Iterable[str]) -> None:
        self._unset_keys = set(keys)

    @override
    def __setitem__(self, key: str, value: Any) -> None:
        # A later concrete assignment revokes an earlier unset marker. During
        # ``dict.__init__`` the set may not exist yet.
        unset_keys = getattr(self, "_unset_keys", None)
        if unset_keys is not None:
            unset_keys.discard(key)
        super().__setitem__(key, value)

    @override
    def copy(self) -> LayeredDict:
        return LayeredDict(super().copy(), unset_keys=self._unset_keys)


EnvValue = str | HiddenValue


class ArgumentMeta(Generic[T]):
    description: str
    default: Callable[[Config], T]
    handler: Callable[[Config, T], T] | None

    def __init__(self, description, default, handler=None) -> None:
        self.description = description
        self.default = default
        self.handler = handler


# Connector arguments
# These are arguments passed to the various connectors that provide the underlying
# API to read/write external systems.


# Note: ConnectorArguments is specifically not total as it's used to type many
# functions via Unpack and we don't want to specify every kwarg.
class ConnectorArguments(TypedDict, total=False):
    # Auth arguments
    _sudo: bool
    _sudo_user: str
    _use_sudo_login: bool
    _sudo_password: str
    _preserve_sudo_env: bool
    _sudo_set_home: bool
    _su_user: str
    _use_su_login: bool
    _preserve_su_env: bool
    _su_shell: str
    _su_password: str
    _doas: bool
    _doas_user: str
    _dzdo: bool
    _dzdo_user: str

    # Shell arguments
    _shell_executable: str
    _chdir: str
    _env: Mapping[str, EnvValue]

    # Connector control (outside of command generation)
    _success_exit_codes: Iterable[int]
    _timeout: int
    _get_pty: bool
    _stdin: str | list[str] | Iterable[str]

    # Retry arguments
    _retries: int
    _retry_delay: int | float
    _retry_until: Callable[[dict], bool]

    # Temp directory argument
    _temp_dir: str


def merge_env_layers(
    base: Mapping[str, EnvValue] | None,
    override: Mapping[str, EnvValue] | None,
) -> LayeredDict:
    """
    Merge nested ``_env`` mappings while preserving explicit removals.

    ``None`` and :data:`UNSET` environment values both remove a key inherited
    from an outer layer. The removal is retained in :attr:`LayeredDict.unset_keys`
    so a later call to :func:`generate_env` does not add the key back from
    ``config.ENV`` or the local process environment.
    """

    merged = LayeredDict()
    unset_keys: set[str] = set()

    if isinstance(base, Mapping):
        merged.update(base)
        unset_keys.update(getattr(base, "unset_keys", frozenset()))

    if isinstance(override, Mapping):
        unset_keys.update(getattr(override, "unset_keys", frozenset()))
        for key, value in override.items():
            if is_unset(value) or value is None:
                merged.pop(key, None)
                unset_keys.add(key)
            else:
                merged[key] = value
                unset_keys.discard(key)

    for key in unset_keys:
        merged.pop(key, None)

    merged.unset_keys = frozenset(unset_keys)
    return merged


def generate_env(config: Config, value: Mapping[str, EnvValue] | None) -> LayeredDict:
    env: LayeredDict = LayeredDict(
        {key: os.environ[key] for key in config.INHERIT_ENV if key in os.environ}
    )
    env.update(config.ENV)

    unset_keys: set[str] = set(getattr(value, "unset_keys", frozenset()))

    if value is not None:
        for key, env_value in value.items():
            if is_unset(env_value) or env_value is None:
                env.pop(key, None)
                unset_keys.add(key)
            else:
                env[key] = env_value
                unset_keys.discard(key)

    for key in unset_keys:
        env.pop(key, None)

    env.unset_keys = frozenset(unset_keys)
    return env


def merge_argument_layers(
    base: Mapping[str, Any] | None,
    override: Mapping[str, Any] | None,
) -> dict[str, Any]:
    """
    Copy and merge two host/deploy argument scopes without mutating either.

    :data:`UNSET` values and :class:`LayeredDict` unset metadata become tombstone
    values in the resulting scope. :func:`pop_global_arguments` turns those into
    resolved defaults while recording them for connector filtering.
    """

    merged: dict[str, Any] = dict(base or {})

    override_unset_keys = getattr(override, "unset_keys", frozenset())
    if isinstance(override, Mapping):
        for key, value in override.items():
            if key in override_unset_keys or is_unset(value):
                merged[key] = UNSET
            elif key == "_env" and isinstance(value, Mapping):
                base_env = merged.get("_env")
                merged[key] = merge_env_layers(
                    base_env if isinstance(base_env, Mapping) else None,
                    value,
                )
            else:
                merged[key] = value

    for key in override_unset_keys:
        merged[key] = UNSET

    return merged


def get_connector_arguments(
    global_arguments: Mapping[str, Any],
) -> ConnectorArguments:
    """Return connector args, omitting values explicitly unset in this scope."""

    unset_keys = getattr(global_arguments, "unset_keys", frozenset())
    connector_arguments: dict[str, Any] = {}

    for key in CONNECTOR_ARGUMENT_KEYS:
        if key not in global_arguments or key in unset_keys:
            continue

        value = global_arguments[key]
        if is_unset(value):
            continue

        if key == "_env" and isinstance(value, Mapping):
            value = {
                env_key: env_value
                for env_key, env_value in value.items()
                if not is_unset(env_value) and env_value is not None
            }

        connector_arguments[key] = value

    return cast(ConnectorArguments, connector_arguments)


auth_argument_meta: dict[str, ArgumentMeta] = {
    "_sudo": ArgumentMeta(
        "Execute/apply any changes with sudo.",
        default=lambda config: config.SUDO,
    ),
    "_sudo_user": ArgumentMeta(
        "Execute/apply any changes with sudo as a non-root user.",
        default=lambda config: config.SUDO_USER,
    ),
    "_use_sudo_login": ArgumentMeta(
        "Execute sudo with a login shell.",
        default=lambda config: config.USE_SUDO_LOGIN,
    ),
    "_sudo_password": ArgumentMeta(
        "Password to sudo with. If needed and not specified pyinfra will prompt for it.",
        default=lambda config: config.SUDO_PASSWORD,
    ),
    "_preserve_sudo_env": ArgumentMeta(
        "Preserve the shell environment of the connecting user when using sudo.",
        default=lambda config: config.PRESERVE_SUDO_ENV,
    ),
    "_sudo_set_home": ArgumentMeta(
        "Set ``HOME`` to the target user's home directory when using sudo (``sudo -H``).",
        default=lambda config: config.SUDO_SET_HOME,
    ),
    "_su_user": ArgumentMeta(
        "Execute/apply any changes with this user using su.",
        default=lambda config: config.SU_USER,
    ),
    "_use_su_login": ArgumentMeta(
        "Execute su with a login shell.",
        default=lambda config: config.USE_SU_LOGIN,
    ),
    "_preserve_su_env": ArgumentMeta(
        "Preserve the shell environment of the connecting user when using su.",
        default=lambda config: config.PRESERVE_SU_ENV,
    ),
    "_su_shell": ArgumentMeta(
        "Use this shell (instead of user login shell) when using ``_su``). "
        + "Only available under Linux, for use when using `su` with a user that "
        + "has nologin/similar as their login shell.",
        default=lambda config: config.SU_SHELL,
    ),
    "_su_password": ArgumentMeta(
        "Password to su with.",
        default=lambda config: config.SU_PASSWORD,
    ),
    "_doas": ArgumentMeta(
        "Execute/apply any changes with doas.",
        default=lambda config: config.DOAS,
    ),
    "_doas_user": ArgumentMeta(
        "Execute/apply any changes with doas as a non-root user.",
        default=lambda config: config.DOAS_USER,
    ),
    "_dzdo": ArgumentMeta(
        "Execute/apply any changes with dzdo.",
        default=lambda config: config.DZDO,
    ),
    "_dzdo_user": ArgumentMeta(
        "Execute/apply any changes with dzdo as a non-root user.",
        default=lambda config: config.DZDO_USER,
    ),
}

shell_argument_meta: dict[str, ArgumentMeta] = {
    "_shell_executable": ArgumentMeta(
        "The shell executable to use for executing commands.",
        default=lambda config: config.SHELL,
    ),
    "_chdir": ArgumentMeta(
        "Directory to switch to before executing the command.",
        default=lambda _: None,
    ),
    "_env": ArgumentMeta(
        "Dictionary of environment variables to set.",
        default=lambda _: {},
        handler=generate_env,
    ),
    "_success_exit_codes": ArgumentMeta(
        "List of exit codes to consider a success.",
        default=lambda _: [0],
    ),
    "_timeout": ArgumentMeta(
        "Timeout for *each* command executed during the operation.",
        default=lambda _: None,
    ),
    "_get_pty": ArgumentMeta(
        "Whether to get a pseudoTTY when executing any commands.",
        default=lambda _: False,
    ),
    "_stdin": ArgumentMeta(
        "String or buffer to send to the stdin of any commands.",
        default=lambda _: None,
    ),
    "_temp_dir": ArgumentMeta(
        "Temporary directory on the remote host for file operations.",
        default=lambda config: config.TEMP_DIR,
    ),
}


# Meta arguments
# These provide/extend additional operation metadata


class MetaArguments(TypedDict):
    name: str
    _ignore_errors: bool
    _continue_on_error: bool
    _if: list[Callable[[], bool]] | Callable[[], bool] | None


meta_argument_meta: dict[str, ArgumentMeta] = {
    # NOTE: name is the only non-_-prefixed argument
    "name": ArgumentMeta(
        (
            "Human-readable label for the operation, shown in CLI output and used to identify "
            "the operation in the execution order. Does not affect what is run. If omitted, "
            "pyinfra generates a label from the operation's call signature."
        ),
        default=lambda _: None,
    ),
    "_ignore_errors": ArgumentMeta(
        "Ignore errors when executing the operation.",
        default=lambda config: config.IGNORE_ERRORS,
    ),
    "_continue_on_error": ArgumentMeta(
        (
            "Continue executing operation commands after error. "
            "Only applies when ``_ignore_errors`` is true."
        ),
        default=lambda _: False,
    ),
    "_if": ArgumentMeta(
        "Only run this operation if these functions return True",
        default=lambda _: [],
    ),
}


# Execution arguments
# These alter how pyinfra is to execute an operation. Notably these must all have the same value
# over every target host for the same operation.


class ExecutionArguments(TypedDict):
    _parallel: int
    _run_once: bool
    _serial: bool


execution_argument_meta: dict[str, ArgumentMeta] = {
    "_parallel": ArgumentMeta(
        (
            "Maximum number of hosts to execute this operation on at once. ``0`` (the default) "
            "means use the global ``config.PARALLEL`` value, which itself defaults to *all hosts "
            "in parallel*, capped by the system's open-file-descriptor limit."
        ),
        default=lambda config: config.PARALLEL,
    ),
    "_run_once": ArgumentMeta(
        "Only execute this operation once, on the first host to see it.",
        default=lambda _: False,
    ),
    "_serial": ArgumentMeta(
        "Run this operation host by host, rather than in parallel.",
        default=lambda _: False,
    ),
}


class AllArguments(ConnectorArguments, MetaArguments, ExecutionArguments):
    pass


def all_global_arguments() -> list[tuple[str, type]]:
    """Return all global arguments and their types."""
    return list(get_type_hints(AllArguments).items())


# Create a dictionary for retry arguments
retry_argument_meta: dict[str, ArgumentMeta] = {
    "_retries": ArgumentMeta(
        "Number of times to retry failed operations.",
        default=lambda config: config.RETRY,
    ),
    "_retry_delay": ArgumentMeta(
        "Delay in seconds between retry attempts.",
        default=lambda config: config.RETRY_DELAY,
    ),
    "_retry_until": ArgumentMeta(
        "Callable taking output data that returns True to continue retrying.",
        default=lambda config: None,
    ),
}

all_argument_meta: dict[str, ArgumentMeta] = {
    **auth_argument_meta,
    **shell_argument_meta,
    **meta_argument_meta,
    **execution_argument_meta,
    **retry_argument_meta,  # Add retry arguments
}

EXECUTION_KWARG_KEYS = list(ExecutionArguments.__annotations__.keys())
CONNECTOR_ARGUMENT_KEYS = list(ConnectorArguments.__annotations__.keys())

__argument_docs__ = {
    "Privilege & user escalation": (
        auth_argument_meta,
        """
        .. caution::
            When combining privilege escalation arguments it is important to know the order they
            are applied: ``doas`` -> ``dzdo`` -> ``sudo`` -> ``su``. For example
            ``_sudo=True,_su_user="pyinfra"`` yields a command like ``sudo su pyinfra..``.
        """,
        """
        .. code:: python

            # Execute a command with sudo
            server.user(
                name="Create pyinfra user using sudo",
                user="pyinfra",
                _sudo=True,
            )

            # Execute a command with a specific sudo password
            server.user(
                name="Create pyinfra user using sudo",
                user="pyinfra",
                _sudo=True,
                _sudo_password="my-secret-password",
            )
        """,
    ),
    "Shell control & features": (
        shell_argument_meta,
        "",
        """
        .. code:: python

            # Execute from a specific directory
            server.shell(
                name="Bootstrap nginx params",
                commands=["openssl dhparam -out dhparam.pem 4096"],
                _chdir="/etc/ssl/certs",
            )
        """,
    ),
    "Operation meta & callbacks": (meta_argument_meta, "", ""),
    "Execution strategy": (
        execution_argument_meta,
        """
        By default, every operation runs against **all hosts in parallel** (capped by the open-file
        limit). ``_parallel`` lowers that cap for a single operation, ``_serial`` forces host-by-host
        execution, and ``_run_once`` executes only against the first host that reaches the operation.
        These three are mutually exclusive on a per-operation basis and must take the same value on
        every host.
        """,
        "",
    ),
    "Retry behavior": (
        retry_argument_meta,
        """
        Retry arguments allow you to automatically retry operations that fail. You can specify
        how many times to retry, the delay between retries, and optionally a condition
        function to determine when to stop retrying.
        """,
        """
        .. code:: python

            # Retry a command up to 3 times with the default 5 second delay
            server.shell(
                name="Run flaky command with retries",
                commands=["flaky_command"],
                _retries=3,
            )
            # Retry with a custom delay
            server.shell(
                name="Run flaky command with custom delay",
                commands=["flaky_command"],
                _retries=2,
                _retry_delay=10,  # 10 second delay between retries
            )
            # Retry with a custom condition
            def retry_on_specific_error(output_data):
                # Retry if stderr contains "temporary failure"
                for line in output_data["stderr_lines"]:
                    if "temporary failure" in line.lower():
                        return True
                return False

            server.shell(
                name="Run command with conditional retry",
                commands=["flaky_command"],
                _retries=5,
                _retry_until=retry_on_specific_error,
            )
        """,
    ),
}


def pop_global_arguments(
    state: State,
    host: Host,
    kwargs: dict[str, Any],
) -> tuple[AllArguments, list[str]]:
    """
    Pop and return operation global keyword arguments, in preferred order:

    + From the current context (a direct @operator or @deploy function being called)
    + From any current @deploy context (deploy kwargs)
    + From the host data variables
    + From the config variables
    """

    config = state.config
    if ctx_config.isset():
        config = ctx_config.get()
        assert config is not None

    cdkwargs = host.current_deploy_kwargs
    meta_kwargs: dict[str, Any] = cdkwargs or {}  # type: ignore[assignment]
    meta_unset_keys = getattr(meta_kwargs, "unset_keys", frozenset())
    caller_unset_keys = getattr(kwargs, "unset_keys", frozenset())

    # Nested ``_env`` mappings are layered rather than replaced so an inner scope
    # can remove one inherited variable without losing all the others.
    explicit_env = kwargs.get("_env", default_sentinel)
    if isinstance(explicit_env, Mapping):
        meta_env = meta_kwargs.get("_env")
        kwargs["_env"] = merge_env_layers(
            meta_env if isinstance(meta_env, Mapping) else None,
            explicit_env,
        )

    arguments = LayeredDict()
    found_keys: list[str] = []
    unset_keys: set[str] = set()

    for key, type_ in all_global_arguments():
        argument_meta = all_argument_meta[key]
        handler = argument_meta.handler
        config_default: Any = argument_meta.default(config)
        default = config_default

        host_default = getattr(host.data, key, default_sentinel)
        if host_default is not default_sentinel:
            default = host_default

        caller_marked_unset = key in caller_unset_keys
        if key in kwargs:
            found_keys.append(key)
            value = kwargs.pop(key)
            if is_unset(value):
                caller_marked_unset = True
        elif key in meta_kwargs:
            value = meta_kwargs[key]
        else:
            value = default

        if caller_marked_unset or key in meta_unset_keys or is_unset(value):
            # Resolve ordinary engine behaviour from the config default, while
            # recording that the connector argument itself was explicitly
            # unset and must therefore be omitted.
            unset_keys.add(key)
            value = config_default

        if handler:
            value = handler(config, value)

        if value != default:
            raise_if_bad_type(
                value,
                type_,
                ArgumentTypeError,
                f"Invalid argument `{key}`:",
            )

        # TODO: why is type failing here?
        arguments[key] = value  # type: ignore

    arguments.unset_keys = frozenset(unset_keys)
    return cast(AllArguments, arguments), found_keys
