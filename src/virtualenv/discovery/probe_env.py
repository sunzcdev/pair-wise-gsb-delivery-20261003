"""Choose which of the caller's environment variables reach the interpreter probe.

The probe is the subprocess that asks a Python executable for its information. A launcher or CI wrapper can leave
variables such as ``PYTHONHOME`` behind that make an otherwise healthy interpreter answer with another prefix, or fail
to start at all. The policies here let a caller decide what the probe inherits, while the executable the project names
and the options the creator receives stay exactly as given.
"""

from __future__ import annotations

import os
from collections.abc import Callable
from typing import TYPE_CHECKING, Final

if TYPE_CHECKING:
    from collections.abc import Mapping

    from python_discovery import PythonInfo

#: A callable that runs the probe for an executable with a given environment, raising when it cannot.
ProbeCallback = Callable[[str, "Mapping[str, str]"], "PythonInfo"]

#: Hand the whole parent environment to the probe, the behavior of earlier releases.
INHERIT: Final[str] = "inherit"
#: Hand over the parent environment without the variables that steer an interpreter at start-up.
SCRUB: Final[str] = "scrub"
#: Hand over only the variables a process needs to start, so the probe sees a predictable environment.
CLEAN: Final[str] = "clean"
#: Every probe environment policy, from the widest to the narrowest.
POLICIES: Final[tuple[str, ...]] = (INHERIT, SCRUB, CLEAN)

#: Variables that make an interpreter load a different standard library, search path or start-up module.
STEERING_VARIABLES: Final[frozenset[str]] = frozenset({
    "PYTHONHOME",
    "PYTHONPATH",
    "PYTHONSTARTUP",
    "PYTHONEXECUTABLE",
    "PYTHONUSERBASE",
    "PYTHONPLATLIBDIR",
})

#: Variables a process needs to start: the ones Windows requires plus the usual profile and locale ones elsewhere.
ESSENTIAL_VARIABLES: Final[frozenset[str]] = frozenset({
    "PATH",
    "PATHEXT",
    "SYSTEMROOT",
    "SYSTEMDRIVE",
    "WINDIR",
    "COMSPEC",
    "TEMP",
    "TMP",
    "TMPDIR",
    "NUMBER_OF_PROCESSORS",
    "PROCESSOR_ARCHITECTURE",
    "PROCESSOR_IDENTIFIER",
    "OS",
    "USERPROFILE",
    "HOMEDRIVE",
    "HOMEPATH",
    "APPDATA",
    "LOCALAPPDATA",
    "PROGRAMDATA",
    "HOME",
    "LANG",
    "LANGUAGE",
    "LC_ALL",
    "LC_CTYPE",
    "TZ",
    "USER",
    "LOGNAME",
})

#: The failures a probe reports: a query that did not answer, or an executable that could not be started.
PROBE_ERRORS: Final[tuple[type[BaseException], ...]] = (RuntimeError, OSError)


def build_probe_env(policy: str, env: Mapping[str, str]) -> dict[str, str]:
    """Return the environment to hand to an interpreter probe under ``policy``.

    The parent mapping is never mutated, so the creator can keep receiving the caller's snapshot unchanged.

    :param policy: one of :data:`POLICIES`
    :param env: the parent environment
    :returns: the environment for the probe subprocess
    :raises ValueError: when the policy is unknown
    """
    if policy == INHERIT:
        return dict(env)
    if policy == SCRUB:
        return {key: value for key, value in env.items() if not _is_steering(key)}
    if policy == CLEAN:
        return {key: value for key, value in env.items() if key.upper() in ESSENTIAL_VARIABLES}
    msg = f"unknown probe environment policy {policy!r}, expected one of {', '.join(POLICIES)}"
    raise ValueError(msg)


def withheld_variables(policy: str, env: Mapping[str, str]) -> tuple[str, ...]:
    """Return the variables of ``env`` that ``policy`` keeps away from the probe, sorted by name.

    :param policy: one of :data:`POLICIES`
    :param env: the parent environment
    :returns: the names of the withheld variables
    """
    probe_env = build_probe_env(policy, env)
    return tuple(sorted(key for key in env if key not in probe_env))


def steering_variables(env: Mapping[str, str]) -> tuple[str, ...]:
    """Return the steering variables ``env`` sets, sorted by name.

    :param env: an environment mapping
    :returns: the names of the steering variables present in ``env``
    """
    return tuple(sorted(key for key in env if _is_steering(key)))


class ProbeEnvironmentError(RuntimeError):
    """A probe failed, recording whether the parent environment or the interpreter is to blame.

    :param executable: the interpreter that could not be queried
    :param cause: the failure the probe reported
    :param policy: the policy the failed probe ran under
    :param steering: the steering variables the parent environment set
    :param recovered: the interpreter information a narrower policy recovered, ``None`` when none did

    :ivar external_env: ``True`` when a narrower policy made the interpreter answer, so a parent variable disturbed the
        probe rather than the interpreter being broken
    """

    def __init__(
        self,
        executable: str,
        cause: BaseException,
        *,
        policy: str,
        steering: tuple[str, ...],
        recovered: PythonInfo | None,
    ) -> None:
        self.executable = executable
        self.policy = policy
        self.steering = steering
        self.recovered = recovered
        self.external_env = recovered is not None
        condition = f" with {', '.join(steering)} set" if steering else ""
        if self.external_env:
            message = (
                f"probing {executable} under the {policy!r} policy failed{condition}, but a narrower policy made it "
                f"answer: the parent environment disturbed the probe, the interpreter itself is healthy"
            )
        else:
            message = (
                f"probing {executable} under the {policy!r} policy failed{condition}, and no narrower policy helped: "
                f"the interpreter cannot report its information"
            )
        super().__init__(message)
        self.__cause__ = cause


def probe_interpreter(
    executable: str,
    *,
    policy: str = INHERIT,
    env: Mapping[str, str] | None = None,
    resolve_to_host: bool = True,
    probe: ProbeCallback | None = None,
) -> PythonInfo:
    """Query one interpreter under ``policy``, classifying a failure instead of returning ``None``.

    A failure under a wider policy is retried with each narrower policy, so the raised error can tell a disturbed probe
    from a broken interpreter. Every attempt probes afresh, since the cache is keyed by the executable alone and would
    otherwise return an answer computed under a different policy.

    :param executable: the interpreter to query, kept exactly as the caller gave it
    :param policy: one of :data:`POLICIES`
    :param env: the parent environment, ``None`` for :data:`os.environ`
    :param resolve_to_host: resolve through virtualenv layers to the system interpreter
    :param probe: the probe to run, ``None`` for the default subprocess interrogation
    :returns: the queried interpreter information
    :raises ProbeEnvironmentError: when the policy cannot query the interpreter
    :raises ValueError: when the policy is unknown
    """
    parent = os.environ if env is None else env
    query = probe if probe is not None else _subprocess_probe(resolve_to_host=resolve_to_host)

    def attempt(one_policy: str) -> PythonInfo:
        return query(executable, build_probe_env(one_policy, parent))

    try:
        return attempt(policy)
    except PROBE_ERRORS as cause:
        recovered = _recover(attempt, policy)
        raise ProbeEnvironmentError(
            executable,
            cause,
            policy=policy,
            steering=steering_variables(parent),
            recovered=recovered,
        ) from cause


def _is_steering(key: str) -> bool:
    return key.upper() in STEERING_VARIABLES


def _recover(attempt: Callable[[str], PythonInfo], policy: str) -> PythonInfo | None:
    # a broken interpreter fails under every policy, a disturbed one answers once the steering variables are gone
    start = POLICIES.index(policy) + 1
    for narrower in POLICIES[start:]:
        try:
            return attempt(narrower)
        except PROBE_ERRORS:
            continue
    return None


def _subprocess_probe(*, resolve_to_host: bool) -> ProbeCallback:
    # imported here so a plain policy query does not load interpreter interrogation
    from python_discovery import PythonInfo  # ruff:ignore[import-outside-top-level]

    def probe(executable: str, env: Mapping[str, str]) -> PythonInfo:
        # no cache and no read of one: an answer computed under another policy would defeat the classification
        result = PythonInfo.from_exe(
            executable,
            None,
            raise_on_error=True,
            ignore_cache=True,
            resolve_to_host=resolve_to_host,
            env=env,
        )
        if result is None:  # pragma: no cover - raise_on_error keeps a failed query from returning None here
            msg = f"{executable} produced no interpreter information"
            raise RuntimeError(msg)
        return result

    return probe


__all__ = [
    "CLEAN",
    "ESSENTIAL_VARIABLES",
    "INHERIT",
    "POLICIES",
    "PROBE_ERRORS",
    "ProbeCallback",
    "ProbeEnvironmentError",
    "SCRUB",
    "STEERING_VARIABLES",
    "build_probe_env",
    "probe_interpreter",
    "steering_variables",
    "withheld_variables",
]

