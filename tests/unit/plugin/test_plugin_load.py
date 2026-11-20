from __future__ import annotations

import logging
import re
from importlib.metadata import EntryPoint, entry_points
from typing import TYPE_CHECKING, Final

import pytest
from python_discovery import PythonInfo

from virtualenv.config.cli.parser import VirtualEnvOptions
from virtualenv.run import session_via_cli
from virtualenv.run.plugin.base import PluginLoader
from virtualenv.run.plugin.creators import CreatorSelector
from virtualenv.run.plugin.discovery import Discovery
from virtualenv.run.plugin.seeders import SeederSelector

if TYPE_CHECKING:
    from collections.abc import Iterator
    from pathlib import Path

    from pytest_mock import MockerFixture

_GROUP: Final[str] = "virtualenv.create"
# a value naming a module that is not installed, which fails to import like a plugin missing a dependency
_BROKEN: Final[str] = "virtualenv_plugin_dependency_missing.creator:Creator"
# a plugin that imports fine but only describes PyPy, so on another interpreter it loads and still cannot create
_UNSUPPORTED: Final[str] = "virtualenv.create.via_global_ref.builtin.pypy.pypy3:PyPy3Posix"

_LOADERS: Final[tuple[type[PluginLoader], ...]] = (CreatorSelector, SeederSelector, Discovery)


class _FakeEntryPoints:
    """The entry points of the process, with extra entries added, or the extra entries alone.

    :param extra: the additional entry points
    :param include_real: ``True`` to keep the entry points of the installed packages

    """

    def __init__(
        self, extra: dict[str, EntryPoint], include_real: bool = True, excluded: frozenset[str] = frozenset()
    ) -> None:
        self._extra = extra
        # the real entry points load the bundled plugins, so the selection has the usual alternatives to fall back to
        self._real = entry_points() if include_real else None
        self._excluded = excluded

    def _for(self, group: str) -> list[EntryPoint]:
        result = [entry for entry in self._extra.values() if entry.group == group]
        if self._real is None or group in self._excluded:
            return result
        return [*result, *self._real.select(group=group)]

    # both signatures exist, as importlib.metadata changed the entry points object over the supported versions
    def select(self, group: str) -> list[EntryPoint]:
        return self._for(group)

    # the signature mirrors importlib.metadata, which callers on Python 3.9 use
    def get(self, group: str, default: object = None) -> list[EntryPoint]:  # ruff:ignore[unused-method-argument]
        return self._for(group)


class FakePlugins:
    """Controls the fake entry points the loaders see, with the caches refreshed after each change."""

    def __init__(self, mocker: MockerFixture) -> None:
        self._mocker = mocker
        self._extra: dict[str, EntryPoint] = {}
        self._include_real = True
        self._excluded: frozenset[str] = frozenset()
        self.refresh()

    def add(self, name: str, value: str, group: str = _GROUP) -> None:
        """Register a fake entry point under the group.

        :param name: the entry point name
        :param value: the entry point value naming the plugin class
        :param group: the entry point group

        """
        self._extra[name] = EntryPoint(name=name, value=value, group=group)
        self.refresh()

    def only_fakes(self) -> None:
        """Drop the installed entry points, so the fake ones are all that a loader sees."""
        self._include_real = False
        self.refresh()

    def exclude_real(self, group: str) -> None:
        """Hide the installed entry points of one group, so a fake of the same name is the only one.

        :param group: the entry point group

        """
        self._excluded |= {group}
        self.refresh()

    def refresh(self) -> None:
        """Install the fake entry points and clear every cache that would otherwise hide them."""
        self._mocker.patch.object(
            PluginLoader,
            "_ENTRY_POINTS",
            _FakeEntryPoints(dict(self._extra), include_real=self._include_real, excluded=self._excluded),
        )
        for loader in _LOADERS:
            # each loader reads its plugin classes once per process
            self._mocker.patch.object(loader, "_OPTIONS", None)
        PluginLoader._LOAD_ERRORS.clear()  # ruff:ignore[private-member-access]  # the cache is per process


@pytest.fixture
def fake_plugins(mocker: MockerFixture) -> Iterator[FakePlugins]:
    # the load errors have no patched seam, so a leftover from one test would leak into the next one
    before = dict(PluginLoader._LOAD_ERRORS)  # ruff:ignore[private-member-access]
    yield FakePlugins(mocker)
    PluginLoader._LOAD_ERRORS.clear()  # ruff:ignore[private-member-access]
    PluginLoader._LOAD_ERRORS.update(before)  # ruff:ignore[private-member-access]


@pytest.fixture
def two_fakes(fake_plugins: FakePlugins) -> FakePlugins:
    """A plugin that fails to load and one that loads but does not support this interpreter."""
    fake_plugins.add("fake-broken", _BROKEN)
    fake_plugins.add("fake-unsupported", _UNSUPPORTED)
    return fake_plugins


def _create(tmp_path: Path, creator: str | None = None) -> VirtualEnvOptions:
    args = [str(tmp_path / "env"), "--no-seed", "--activators="]
    if creator is not None:
        args.extend(("--creator", creator))
    options = VirtualEnvOptions()
    session_via_cli(args, options=options, setup_logging=False)
    return options


@pytest.mark.usefixtures("two_fakes")
def test_help_lists_plugins_that_load(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as context:
        session_via_cli(["--help"], setup_logging=False)

    out = capsys.readouterr().out
    assert context.value.code == 0
    # the help still generates, and offers exactly the creators that can create for this interpreter
    assert _creator_choices(out) == list(CreatorSelector.for_interpreter(PythonInfo.current_system()).key_to_class)
    # the plugin that fails to load, and the one that does not support this interpreter, are not offered
    assert "fake-broken" not in out
    assert "fake-unsupported" not in out


def _creator_choices(help_text: str) -> list[str]:
    # the usage line and the option line both carry the "{a,b,c}" choice list; the first one is enough
    match = re.search(r"--creator \{([^}]*)\}", help_text)
    assert match is not None, help_text
    return match.group(1).split(",")


@pytest.mark.usefixtures("two_fakes")
def test_auto_mode_selects_a_compatible_creator(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.WARNING, logger="virtualenv.run.plugin.base")
    options = _create(tmp_path)

    # the two fake creators are unusable, so the automatic choice lands on one of the bundled creators instead
    assert options.creator not in {"fake-broken", "fake-unsupported"}
    assert options.creator in CreatorSelector.for_interpreter(PythonInfo.current_system()).key_to_class
    # the user is told why the plugin is missing even when no option names it
    assert "fake-broken" in caplog.text


@pytest.mark.usefixtures("two_fakes")
def test_broken_plugin_reports_the_load_error(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.WARNING, logger="virtualenv.run.plugin.base")
    with pytest.raises(RuntimeError) as context:
        _create(tmp_path, "fake-broken")

    message = str(context.value)
    assert "fake-broken" in message
    assert "failed to load" in message
    assert "ModuleNotFoundError" in message
    assert _BROKEN in message
    # the original error is kept as the cause rather than a compatible creator silently taking over
    assert isinstance(context.value.__cause__, ModuleNotFoundError)
    # the load failure was already reported when the entry points were read
    assert "fake-broken" in caplog.text


@pytest.mark.usefixtures("two_fakes")
def test_unsupported_plugin_is_not_reported_as_a_load_error(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.WARNING, logger="virtualenv.run.plugin.base")
    with pytest.raises(RuntimeError) as context:
        _create(tmp_path, "fake-unsupported")

    message = str(context.value)
    spec = PythonInfo.current_system().spec
    # the plugin loaded, so the reason is the interpreter rather than a missing dependency
    assert message == f"creator 'fake-unsupported' cannot be used for {spec}: does not support this interpreter"
    assert context.value.__cause__ is None
    assert "fake-unsupported" not in caplog.text


@pytest.mark.usefixtures("two_fakes")
def test_load_failure_is_logged_at_warning(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.WARNING, logger="virtualenv.run.plugin.base")

    CreatorSelector.for_interpreter(PythonInfo.current_system())

    assert [record.levelno for record in caplog.records] == [logging.WARNING]
    assert "fake-broken" in caplog.text
    assert "ModuleNotFoundError" in caplog.text


@pytest.mark.usefixtures("two_fakes")
def test_unknown_plugin_keeps_the_not_installed_message(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    # argparse rejects an unknown name on the command line before selection, so this path is reached from an env var
    monkeypatch.setenv("VIRTUALENV_CREATOR", "fake-typo")
    with pytest.raises(RuntimeError) as context:
        session_via_cli([str(tmp_path / "env"), "--no-seed"], setup_logging=False)

    spec = PythonInfo.current_system().spec
    available = ", ".join(sorted(CreatorSelector.for_interpreter(PythonInfo.current_system()).key_to_class))
    assert str(context.value) == (
        f"creator 'fake-typo' is not available for {spec}. Available creators: {available}. Is the plugin installed?"
    )


def test_no_usable_creator_names_the_load_failure(two_fakes: FakePlugins) -> None:
    two_fakes.only_fakes()

    with pytest.raises(RuntimeError) as context:
        CreatorSelector.for_interpreter(PythonInfo.current_system())

    message = str(context.value)
    # nothing that loaded can create here, so the message names the plugin and the import that failed
    assert "fake-broken" in message
    assert "ModuleNotFoundError" in message


@pytest.mark.usefixtures("two_fakes")
def test_broken_seeder_reports_the_load_error(fake_plugins: FakePlugins, tmp_path: Path) -> None:
    fake_plugins.add("fake-broken-seed", _BROKEN, group="virtualenv.seed")

    with pytest.raises(RuntimeError) as context:
        session_via_cli([str(tmp_path / "env"), "--seeder", "fake-broken-seed"], setup_logging=False)

    # the same load message and cause apply to every plugin group, not only to creators
    assert "virtualenv.seed plugin 'fake-broken-seed'" in str(context.value)
    assert "ModuleNotFoundError" in str(context.value)
    assert isinstance(context.value.__cause__, ModuleNotFoundError)


def test_auto_mode_falls_back_when_the_default_seeder_is_broken(fake_plugins: FakePlugins, tmp_path: Path) -> None:
    fake_plugins.add("extra-seed", _BROKEN, group="virtualenv.seed")
    options = VirtualEnvOptions()

    # the app-data seeder still loads, so the default stays on it while the broken extra seeder is only logged
    session_via_cli([str(tmp_path / "env"), "--no-seed"], options=options, setup_logging=False)

    assert options.seeder == "app-data"
    assert "extra-seed" in SeederSelector.load_errors_for("virtualenv.seed")


def test_seeder_default_falls_back_when_app_data_is_broken(fake_plugins: FakePlugins, tmp_path: Path) -> None:
    # the installed seeders are hidden so the bundled app-data seeder cannot load and take over the fake name
    fake_plugins.exclude_real("virtualenv.seed")
    fake_plugins.add("pip", "virtualenv.seed.embed.pip_invoke:PipInvoke", group="virtualenv.seed")
    fake_plugins.add("app-data", _BROKEN, group="virtualenv.seed")
    options = VirtualEnvOptions()

    # nothing names a seeder, so the default must land on one that loaded rather than on the missing app-data seeder
    session_via_cli([str(tmp_path / "env"), "--no-seed"], options=options, setup_logging=False)

    assert options.seeder == "pip"


def test_broken_discovery_reports_the_load_error(fake_plugins: FakePlugins, tmp_path: Path) -> None:
    fake_plugins.add("fake-broken-discovery", _BROKEN, group="virtualenv.discovery")

    with pytest.raises(RuntimeError) as context:
        session_via_cli([str(tmp_path / "env"), "--discovery", "fake-broken-discovery"], setup_logging=False)

    assert "virtualenv.discovery plugin 'fake-broken-discovery'" in str(context.value)
    assert isinstance(context.value.__cause__, ModuleNotFoundError)
