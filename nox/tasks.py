# Copyright 2017 Alethea Katherine Flowers
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

__lazy_modules__ = {
    "ast",
    "colorlog.escape_codes",
    "importlib.util",
    "json",
    "nox",
    "nox._resolver",
    "nox._version",
    "nox.logger",
    "nox.manifest",
}

import ast
import contextlib
import importlib.util
import json
import os
import sys
from collections.abc import Sequence
from typing import TYPE_CHECKING, TypeVar

from colorlog.escape_codes import parse_colors

import nox
from nox import _merge, registry
from nox._resolver import CycleError
from nox._version import InvalidVersionSpecifier, VersionCheckFailed, check_nox_version
from nox.logger import logger
from nox.manifest import WARN_PYTHONS_IGNORED, Manifest
from nox.sessions import Result, Status, _duration_str, resolve_allow_parallel

if TYPE_CHECKING:
    import types
    from collections.abc import Generator

    from nox._options import NoxConfig

__all__ = [
    "create_report",
    "discover_manifest",
    "filter_manifest",
    "final_reduce",
    "honor_list_request",
    "honor_usage_request",
    "load_nox_module",
    "merge_noxfile_options",
    "print_summary",
    "run_manifest",
]


def __dir__() -> list[str]:
    return __all__


def _module_origin(module: types.ModuleType) -> str | None:
    """Return the source path a module was (or is being) loaded from.

    A half-initialised module left behind by a failed Noxfile import may not
    yet have ``__path__``; ``__file__`` works for regular modules and for the
    package's ``__init__``. Namespace packages only carry ``__path__``.
    """
    origin = getattr(module, "__file__", None)
    if isinstance(origin, str):
        return origin
    paths = getattr(module, "__path__", None)
    if isinstance(paths, Sequence) and paths:
        first = next(iter(paths))
        if isinstance(first, str):
            return first
    return None


@contextlib.contextmanager
def _isolated_noxfile_import(noxfile_parent_dir: str) -> Generator[None, None, None]:
    """Keep one Noxfile's module discovery from leaking into later runs.

    When Nox's Python API is driven several times in one process (e.g. a
    daemon running the Noxfiles of several tool directories), every run would
    otherwise share import state:

    * the Noxfile directory is prepended to ``sys.path`` and stays there, so a
      later project can import the previous project's local modules;
    * the Noxfile itself is always registered as ``user_nox_module``, and any
      local modules it imports (commonly a per-project ``helper.py``) are
      cached in ``sys.modules`` under plain names, so a later project reuses
      the cached copy instead of its own -- even if an earlier run failed
      halfway through its Noxfile.

    On entry the path/module state is snapshotted. On exit (including via an
    exception) the path is restored, pre-existing module entries are put back,
    and modules newly discovered underneath the Noxfile directory are evicted
    from ``sys.modules``, so the next run rediscovers everything from its own
    directory. Installed third-party packages and Nox itself are never touched.
    """
    original_path = list(sys.path)
    if noxfile_parent_dir not in sys.path:
        sys.path.insert(0, noxfile_parent_dir)

    parent_real = os.path.realpath(noxfile_parent_dir)

    def _is_local_module(name: str, module: types.ModuleType | None) -> bool:
        if name == "user_nox_module":
            return True
        origin = _module_origin(module) if module is not None else None
        return origin is not None and os.path.realpath(origin).startswith(
            parent_real + os.sep
        )

    # Entries that already existed and point at local code: restore them on
    # exit. Anything local that appears later was imported by this Noxfile and
    # must be dropped instead, so a subsequent run in another directory cannot
    # resolve the stale cached copy.
    previous_entries = {
        name: module
        for name, module in sys.modules.items()
        if _is_local_module(name, module)
    }

    try:
        yield
    finally:
        sys.path[:] = original_path
        for name in list(sys.modules):
            if not _is_local_module(name, sys.modules.get(name)):
                continue
            if name in previous_entries:
                sys.modules[name] = previous_entries[name]
            else:
                sys.modules.pop(name, None)


def _load_and_exec_nox_module(global_config: NoxConfig) -> types.ModuleType:
    """
    Loads, executes, then returns the global_config Nox module.

    Args:
        global_config (Namespace): The global config.

    Raises:
        IOError: If the Nox module cannot be loaded. This
            exception is chosen such that it will be caught
            by load_nox_module and logged appropriately.

    Returns:
        types.ModuleType: The initialised Nox module.
    """
    spec = importlib.util.spec_from_file_location(
        "user_nox_module", str(global_config.noxfile)
    )
    assert spec is not None  # If None, fatal importlib error, would crash anyway

    module = importlib.util.module_from_spec(spec)
    assert module is not None  # If None, fatal importlib error, would crash anyway

    loader = spec.loader
    assert loader is not None  # If None, fatal importlib error, would crash anyway

    noxfile_parent_dir = os.path.dirname(global_config.noxfile)
    # See https://docs.python.org/3/library/importlib.html#importing-a-source-file-directly
    # The import context isolates both the module cache and sys.path so that
    # driving another Noxfile later in the same process rediscovers its own
    # modules. The Noxfile module is registered under its fixed name only for
    # the duration of execution, after which the previous state is restored.
    with _isolated_noxfile_import(noxfile_parent_dir):
        sys.modules["user_nox_module"] = module
        loader.exec_module(module)
    return module


def load_nox_module(global_config: NoxConfig) -> types.ModuleType | int:
    """Load the user's Noxfile and return the module object for it.

    .. note::

        This task has two side effects; it makes ``global_config.noxfile``
        an absolute path, and changes the working directory of the process.

    Args:
        global_config (.nox.main.GlobalConfig): The global config.

    Returns:
        module: The module designated by the Noxfile path.
    """
    # Be sure to expand variables
    global_config_noxfile = os.path.expandvars(global_config.noxfile)

    # Make sure we only expand the parent dir just in case the noxfile is a symlink
    noxfile_parent_dir = os.path.realpath(os.path.dirname(global_config_noxfile))

    # Save the absolute path to the Noxfile.
    # This will inoculate it if Nox changes paths because of an implicit
    # or explicit chdir (like the one below).
    global_config.noxfile = os.path.join(
        noxfile_parent_dir, os.path.basename(global_config_noxfile)
    )

    # Each Noxfile load starts from a clean session registry. Without this,
    # driving another Noxfile later in the same process would discover the
    # previous project's sessions alongside (or instead of) its own. The
    # process-level save/restore lives in nox._cli.execute_workflow via
    # nox.registry.isolated(), so callers that already populated the registry
    # outside of a workflow still get it back when the workflow finishes.
    registry.reset()

    try:
        # Check ``nox.needs_version`` by parsing the AST.
        check_nox_version(global_config.noxfile)

        # Move to the path where the Noxfile is.
        # This will ensure that the Noxfile's path is on sys.path, and that
        # import-time path resolutions work the way the Noxfile author would
        # guess. The original working directory (the directory that Nox was
        # invoked from) gets stored by the .invoke_from "option" in _options.
        os.chdir(noxfile_parent_dir)

    except (VersionCheckFailed, InvalidVersionSpecifier) as error:
        logger.error(str(error))
        return 2
    except FileNotFoundError:
        logger.error(
            f"Failed to load Noxfile {global_config.noxfile}, no such file exists."
        )
        return 2
    except OSError:
        logger.exception(f"Failed to load Noxfile {global_config.noxfile}")
        return 2

    return _load_and_exec_nox_module(global_config)


def merge_noxfile_options(
    module: types.ModuleType, global_config: NoxConfig
) -> types.ModuleType:
    """Merges any modifications made to ``nox.options`` by the Noxfile module
    into global_config.

    Args:
        module (module): The Noxfile module.
        global_config (~nox.main.GlobalConfig): The global configuration.
    """
    _merge.merge_noxfile_options(global_config, nox.options)
    return module


def discover_manifest(
    module: types.ModuleType | int, global_config: NoxConfig
) -> Manifest:
    """Discover all session functions in the Noxfile module.

    Args:
        module (module): The Noxfile module.
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        ~.Manifest: A manifest of session functions.
    """
    # Find any function added to the session registry (meaning it was
    # decorated with @nox.session); do not sort these, as they are being
    # sorted by decorator call time.
    functions = registry.get()

    # Get the docstring from the Noxfile
    module_docstring = module.__doc__

    # Return the final dictionary of session functions.
    return Manifest(functions, global_config, module_docstring)


def filter_manifest(manifest: Manifest, global_config: NoxConfig) -> Manifest | int:
    """Filter the manifest according to the provided configuration.

    Args:
        manifest (~.Manifest): The manifest of sessions to be run.
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        Union[~.Manifest,int]: ``3`` if a specified session is not found,
            the manifest otherwise (to be sent to the next task).

    """
    # Shouldn't happen unless the Noxfile is empty
    if not manifest:
        logger.error(f"No sessions found in {global_config.noxfile}.")
        return 3

    # Filter by the name of any explicit sessions.
    # This can raise KeyError if a specified session does not exist;
    # log this if it happens. The sessions does not come from the Noxfile
    # if keywords is not empty or tags are supplied.
    if global_config.tags is None and not global_config.keywords:
        if global_config.sessions is None:
            manifest.filter_by_default()
        else:
            try:
                manifest.filter_by_name(global_config.sessions)
            except KeyError as exc:
                logger.error("Error while collecting sessions.")
                logger.error(exc.args[0])
                return 3

    if not manifest and not global_config.list_sessions:
        print("No sessions selected. Please select a session with -s <session name>.\n")
        _produce_listing(manifest, global_config)
        return 0

    # Filter by python interpreter versions.
    if global_config.pythons:
        manifest.filter_by_python_interpreter(global_config.pythons)
        if not manifest and not global_config.list_sessions:
            logger.error("Python version selection caused no sessions to be selected.")
            return 3

    # Filter by tags.
    if global_config.tags is not None:
        manifest.filter_by_tags(global_config.tags)
        if not manifest and not global_config.list_sessions:
            logger.error("Tag selection caused no sessions to be selected.")
            return 3

    # Filter by keywords.
    if global_config.keywords:
        try:
            ast.parse(global_config.keywords, mode="eval")
        except SyntaxError:
            logger.error(
                "Error while collecting sessions: keywords argument must be a Python"
                " expression."
            )
            return 3

        # This function never errors, but may cause an empty list of sessions
        # (which is an error condition later).
        manifest.filter_by_keywords(global_config.keywords)

    if not manifest and not global_config.list_sessions:
        logger.error("No sessions selected after filtering by keyword.")
        return 3

    # Add dependencies, unless --no-dependencies limits the run to only the
    # explicitly selected sessions (as the parallel runner's children do).
    # Even then, a ``requires=`` entry naming a session that doesn't exist is
    # still an error, the prerequisites just aren't queued.
    try:
        if global_config.no_dependencies:
            for session, selected in manifest.list_all_sessions():
                if selected:
                    list(session.get_direct_dependencies())
        else:
            manifest.add_dependencies()
    except (KeyError, CycleError) as exc:
        logger.error("Error while resolving session dependencies.")
        logger.error(exc.args[0])
        return 3

    # Return the modified manifest.
    return manifest


def _produce_listing(manifest: Manifest, global_config: NoxConfig) -> None:
    # If the user just asked for a list of sessions, print that
    # and any docstring specified in noxfile.py and be done. This
    # can also be called if Noxfile sessions is an empty list.

    if manifest.module_docstring:
        print(manifest.module_docstring.strip(), end="\n\n")

    print(f"Sessions defined in {global_config.noxfile}:\n")

    reset = parse_colors("reset") if global_config.color else ""
    selected_color = parse_colors("cyan") if global_config.color else ""
    skipped_color = parse_colors("white") if global_config.color else ""

    for session, selected in manifest.list_all_sessions():
        marker = "*" if selected else "-"
        color = selected_color if selected else skipped_color
        tag_color = (
            (parse_colors("purple") if selected else parse_colors("light_purple"))
            if global_config.color
            else ""
        )

        tags = f" {tag_color}[{','.join(session.tags)}]{reset}" if session.tags else ""
        description = f" -> {session.description}" if session.description else ""

        print(f"{marker} {color}{session.friendly_name}{reset}{tags}{description}")

    print(
        f"\nsessions marked with {selected_color}*{reset} are selected, sessions marked"
        f" with {skipped_color}-{reset} are skipped."
    )


def _produce_json_listing(manifest: Manifest, global_config: NoxConfig) -> None:  # noqa: ARG001
    report = []
    for session, selected in manifest.list_all_sessions():
        if selected:
            report.append(
                {
                    "session": session.friendly_name,
                    "name": session.name,
                    "description": session.description or "",
                    "python": session.func.python,
                    "tags": session.tags,
                    "call_spec": getattr(session.func, "call_spec", {}),
                }
            )
    print(json.dumps(report, default=str))


def honor_list_request(manifest: Manifest, global_config: NoxConfig) -> Manifest | int:
    """If --list was passed, simply list the manifest and exit cleanly.

    Args:
        manifest (~.Manifest): The manifest of sessions to be run.
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        Union[~.Manifest,int]: ``0`` if a listing is all that is requested,
            the manifest otherwise (to be sent to the next task).
    """
    if not (global_config.list_sessions or global_config.json):
        return manifest

    # JSON output requires list sessions also be specified
    if global_config.json and not global_config.list_sessions:
        logger.error("Must specify --list-sessions with --json")
        return 3

    if global_config.json:
        _produce_json_listing(manifest, global_config)
    else:
        _produce_listing(manifest, global_config)

    return 0


def honor_usage_request(manifest: Manifest, global_config: NoxConfig) -> Manifest | int:
    """If --usage was passed, print the full docstring of the session and exit.
    Raise an error if the session does not exist or has no docstring to print.

    Args:
        manifest (~.Manifest): The manifest of sessions to be run.
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        Union[~.Manifest,int]: ``0`` if usage is printed,
            the manifest otherwise (to be sent to the next task).
    """
    if not global_config.usage:
        return manifest

    name = global_config.usage[0]

    # Search all sessions, not just the filtered queue, so that
    # non-default sessions can also be looked up.
    session = None
    for _ in manifest._all_sessions:
        if _.name == name or name in _.signatures:
            session = _
            break

    if session is None:
        logger.error("Session %s not found.", name)
        return 1

    full_description = session.full_description
    if full_description is None:
        logger.error("Session %s has no docstring.", name)
        return 1

    print(full_description)
    return 0


def run_manifest(manifest: Manifest, global_config: NoxConfig) -> list[Result]:
    """Run the full manifest of sessions.

    Args:
        manifest (~.Manifest): The manifest of sessions to be run.
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        tuple[~nox.sessions.Session,~.SessionStatus]: A two-tuple of the
            sessions and the result of each session that was run.
    """
    # When --parallel/-j requests more than one job, hand off to the parallel
    # scheduler, which runs independent sessions in their own subprocesses.
    jobs = global_config.parallel or 1
    if jobs > 1 and not any(
        resolve_allow_parallel(global_config, session.func)
        for session, selected in manifest.list_all_sessions()
        if selected
    ):
        logger.warning(
            "No selected session allows parallel execution; ignoring --parallel"
            " and running sequentially. Opt in with allow_parallel=True or"
            " --allow-parallel."
        )
        jobs = 1
    if jobs > 1:
        # Imported lazily so sequential runs don't pay for the parallel machinery.
        from nox._parallel import run_manifest_parallel  # noqa: PLC0415

        return run_manifest_parallel(manifest, global_config, jobs)

    results = []

    # Iterate over each session in the manifest, and execute it.
    #
    # Note that it is possible for the manifest to be altered in any given
    # iteration.
    for session in manifest:
        # possibly raise warnings associated with this session
        if WARN_PYTHONS_IGNORED in session.func.should_warn:
            logger.warning(
                f"Session {session.name} is set to run with venv_backend='none', "
                "IGNORING its"
                f" python={session.func.should_warn[WARN_PYTHONS_IGNORED]} parametrization. "
            )

        result = session.execute()
        name = session.friendly_name
        status = result.imperfect
        result.log(f"Session {name} {status}.")
        results.append(result)

        # Sanity check: If we are supposed to stop on the first error case,
        # then abort now.
        if not result and global_config.stop_on_first_error:
            return results

    # The entire manifest has been processed; return the results.
    return results


Sequence_Results_T = TypeVar("Sequence_Results_T", bound=Sequence[Result])


def print_summary(
    results: Sequence_Results_T,
    global_config: NoxConfig,
) -> Sequence_Results_T:
    """Print a summary of the results.

    Args:
        results (Sequence[~nox.sessions.Result]): A list of Result objects.
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        results (Sequence[~nox.sessions.Result]): The results passed
            to this function, unmodified.
    """
    # Sanity check: Do not print results if there was only one session run.
    if len(results) <= 1:
        return results

    # Iterate over the results and print the result for each in a
    # human-readable way. In parallel mode the runner records the wall-clock
    # time; otherwise sum the per-session durations (which run back-to-back).
    total_duration = global_config.parallel_wall_time
    if total_duration is None:
        total_duration = sum(result.duration for result in results)
    duration_str = _duration_str(total_duration, " in {time}")
    logger.session_info(f"Ran {len(results)} sessions{duration_str}:")
    for result in results:
        name = result.session.friendly_name
        status = result.status.name.lower()
        duration_str = _duration_str(result.duration, ", took {time}")
        if result.status is Status.SKIPPED and result.reason:
            result.log(f"* {name}: {status} ({result.reason}){duration_str}")
        else:
            result.log(f"* {name}: {status}{duration_str}")

    # Return the results that were sent to this function.
    return results


def create_report(
    results: Sequence_Results_T, global_config: NoxConfig
) -> Sequence_Results_T:
    """Write a report to the location designated in the config, if any.

    Args:
        results (Sequence[~nox.sessions.Result]): A list of Result objects
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        results (Sequence[~nox.sessions.Result]): The results passed
            to this function, unmodified.
    """
    # Sanity check: If no JSON report was requested, this is a no-op.
    if global_config.report is None:
        return results

    # Write the JSON report.
    with open(global_config.report, "w", encoding="utf-8") as report_file:
        json.dump(
            {
                "result": int(all(results)),
                "sessions": [result.serialize() for result in results],
            },
            report_file,
            indent=2,
            default=str,
        )

    # Return back the results passed to this task.
    return results


def final_reduce(results: list[Result], global_config: NoxConfig) -> int:  # noqa: ARG001
    """Reduce the results to a final exit code.

    Args:
        results (Sequence[~nox.sessions.Result]): A list of Result objects
        global_config (~nox.main.GlobalConfig): The global configuration.

    Returns:
        int: The final status code; ``0`` for success and ``1`` for failure.
    """
    if not all(results):
        return 1
    return 0
