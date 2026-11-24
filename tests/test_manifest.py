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

import typing
from collections.abc import Sequence
from typing import TYPE_CHECKING, Any
from unittest import mock

import pytest

import nox
from nox._decorators import Func
from nox.manifest import (
    WARN_PYTHONS_IGNORED,
    DependencyVariantError,
    KeywordLocals,
    Manifest,
    _normalize_arg,
    _normalized_session_match,
    _null_session_func,
)

if TYPE_CHECKING:
    from collections.abc import Sequence


def create_mock_sessions() -> dict[str, mock.Mock]:
    sessions = {}
    sessions["foo"] = mock.Mock(spec=(), python=None, venv_backend=None, tags=["baz"])
    sessions["bar"] = mock.Mock(
        spec=(),
        python=None,
        venv_backend=None,
        tags=["baz", "qux"],
    )
    return sessions


def create_mock_config() -> Any:
    cfg = mock.sentinel.MOCKED_CONFIG
    cfg.force_venv_backend = None
    cfg.default_venv_backend = None
    cfg.extra_pythons = None
    cfg.force_pythons = None
    cfg.posargs = []
    return cfg


def test__normalize_arg() -> None:
    assert _normalize_arg('test(foo="bar")') == _normalize_arg('test(foo="bar")')

    # In the case of SyntaxError it should fallback to string
    assert (
        _normalize_arg("datetime.datetime(1990; 2, 18),")
        == "datetime.datetime(1990; 2, 18),"
    )


def test__normalized_session_match() -> None:
    session_mock = mock.MagicMock()
    session_mock.signatures = ['test(foo="bar")']
    assert _normalized_session_match("test(foo='bar')", session_mock)


def test_init() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())

    # Assert that basic properties look correctly.
    assert len(manifest) == 2
    assert manifest["foo"].func is sessions["foo"]
    assert manifest["bar"].func is sessions["bar"]


def test_contains() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())

    # Establish that contains works pre-iteration.
    assert "foo" in manifest
    assert "bar" in manifest
    assert "baz" not in manifest

    # Establish that __contains__ works post-iteration.
    for _session in manifest:
        pass
    assert "foo" in manifest
    assert "bar" in manifest
    assert "baz" not in manifest

    # Establish that sessions themselves work.
    assert manifest["foo"] in manifest


def test_getitem() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())

    # Establish that each session is present, and a made-up session
    # is not.
    assert manifest["foo"].func is sessions["foo"]
    assert manifest["bar"].func is sessions["bar"]
    with pytest.raises(KeyError):
        manifest["baz"]

    # Establish that the sessions are still present even after being
    # consumed by iteration.
    for _session in manifest:
        pass
    assert manifest["foo"].func is sessions["foo"]
    assert manifest["bar"].func is sessions["bar"]


def test_iteration() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())

    # There should be two sessions in the queue.
    assert len(manifest._queue) == 2
    assert len(manifest._consumed) == 0

    # The first item should be our "foo" session.
    foo = next(manifest)
    assert foo.func == sessions["foo"]
    assert foo in manifest._consumed
    assert foo not in manifest._queue
    assert len(manifest._consumed) == 1
    assert len(manifest._queue) == 1

    # The second item should be our "bar" session.
    bar = next(manifest)
    assert bar.func == sessions["bar"]
    assert bar in manifest._consumed
    assert bar not in manifest._queue
    assert len(manifest._consumed) == 2
    assert len(manifest._queue) == 0

    # Continuing past the end raises StopIteration.
    with pytest.raises(StopIteration):
        next(manifest)


def test_len() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    assert len(manifest) == 2
    for _session in manifest:
        assert len(manifest) == 2


def test_filter_by_name() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    manifest.filter_by_name(("foo",))
    assert "foo" in manifest
    assert "bar" not in manifest


def test_filter_by_name_maintains_order() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    manifest.filter_by_name(("bar", "foo"))
    assert [session.name for session in manifest] == ["bar", "foo"]


def test_filter_by_name_not_found() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    with pytest.raises(KeyError):
        manifest.filter_by_name(("baz",))


def test_filter_by_python_interpreter() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    manifest["foo"].func.python = "3.8"
    manifest["bar"].func.python = "3.7"
    manifest.filter_by_python_interpreter(("3.8",))
    assert "foo" in manifest
    assert "bar" not in manifest


def test_filter_by_keyword() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    assert len(manifest) == 2
    manifest.filter_by_keywords("foo or bar")
    assert len(manifest) == 2
    manifest.filter_by_keywords("foo")
    assert len(manifest) == 1
    # Match tags
    manifest.filter_by_keywords("not baz")
    assert len(manifest) == 0


@pytest.mark.parametrize(
    ("tags", "session_count"),
    [
        (["baz", "qux"], 2),
        (["baz"], 2),
        (["qux"], 1),
        (["missing"], 0),
        (["baz", "missing"], 2),
    ],
)
def test_filter_by_tags(tags: Sequence[str], session_count: int) -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    assert len(manifest) == 2
    manifest.filter_by_tags(tags)
    assert len(manifest) == session_count


def test_list_all_sessions_with_filter() -> None:
    sessions = create_mock_sessions()
    manifest = Manifest(sessions, create_mock_config())
    assert len(manifest) == 2
    manifest.filter_by_keywords("foo")
    assert len(manifest) == 1
    all_sessions = list(manifest.list_all_sessions())
    assert len(all_sessions) == 2
    # Only one should be marked as selected.
    assert all_sessions[0][1] is True
    assert all_sessions[1][1] is False


def test_add_session_plain() -> None:
    manifest = Manifest({}, create_mock_config())
    session_func = mock.Mock(spec=(), python=None, venv_backend=None)
    for session in manifest.make_session("my_session", session_func):
        manifest.add_session(session)
    assert len(manifest) == 1


def test_add_session_multiple_pythons() -> None:
    manifest = Manifest({}, create_mock_config())

    def session_func() -> None:
        pass

    func = Func(session_func, python=["3.5", "3.6"])
    for session in manifest.make_session("my_session", func):
        manifest.add_session(session)

    assert len(manifest) == 2


def test_add_session_duplicate_pythons() -> None:
    manifest = Manifest({}, create_mock_config())

    def session_func() -> None:
        pass

    func = Func(session_func, python=["3.6", "3.5", "3.6"])
    for session in manifest.make_session("my_session", func):
        manifest.add_session(session)

    assert len(manifest) == 2
    assert [session.func.python for session in manifest] == ["3.6", "3.5"]


@pytest.mark.parametrize(
    ("python", "extra_pythons", "expected"),
    [
        (None, [], [None]),
        (None, ["3.8"], [None]),
        (None, ["3.8", "3.9"], [None]),
        (False, [], [False]),
        (False, ["3.8"], [False]),
        (False, ["3.8", "3.9"], [False]),
        ("3.5", [], ["3.5"]),
        ("3.5", ["3.8"], ["3.5", "3.8"]),
        ("3.5", ["3.8", "3.9"], ["3.5", "3.8", "3.9"]),
        (["3.5", "3.9"], [], ["3.5", "3.9"]),
        (["3.5", "3.9"], ["3.8"], ["3.5", "3.9", "3.8"]),
        (["3.5", "3.9"], ["3.8", "3.4"], ["3.5", "3.9", "3.8", "3.4"]),
        (["3.5", "3.9"], ["3.5", "3.9"], ["3.5", "3.9"]),
    ],
)
def test_extra_pythons(
    python: list[str] | str | bool | None,
    extra_pythons: list[str],
    expected: list[None] | list[bool] | list[str],
) -> None:
    cfg = create_mock_config()
    cfg.extra_pythons = extra_pythons

    manifest = Manifest({}, cfg)

    def session_func() -> None:
        pass

    func = Func(session_func, python=python)
    for session in manifest.make_session("my_session", func):
        manifest.add_session(session)

    assert expected == [session.func.python for session in manifest._all_sessions]


@pytest.mark.parametrize(
    ("python", "force_pythons", "expected"),
    [
        (None, [], [None]),
        (None, ["3.8"], ["3.8"]),
        (None, ["3.8", "3.9"], ["3.8", "3.9"]),
        (False, [], [False]),
        (False, ["3.8"], ["3.8"]),
        (False, ["3.8", "3.9"], ["3.8", "3.9"]),
        ("3.5", [], ["3.5"]),
        ("3.5", ["3.8"], ["3.5", "3.8"]),
        ("3.5", ["3.8", "3.9"], ["3.5", "3.8", "3.9"]),
        (["3.5", "3.9"], [], ["3.5", "3.9"]),
        (["3.5", "3.9"], ["3.8"], ["3.5", "3.9", "3.8"]),
        (["3.5", "3.9"], ["3.8", "3.4"], ["3.5", "3.9", "3.8", "3.4"]),
        (["3.5", "3.9"], ["3.5", "3.9"], ["3.5", "3.9"]),
    ],
)
def test_force_pythons(
    python: list[str] | str | bool | None,
    force_pythons: list[str],
    expected: list[None] | list[bool] | list[str],
) -> None:
    cfg = create_mock_config()
    cfg.force_pythons = force_pythons
    cfg.extra_pythons = force_pythons

    manifest = Manifest({}, cfg)

    def session_func() -> None:
        pass

    func = Func(session_func, python=python)
    for session in manifest.make_session("my_session", func):
        manifest.add_session(session)

    assert expected == [session.func.python for session in manifest._all_sessions]


def test_add_session_parametrized() -> None:
    manifest = Manifest({}, create_mock_config())

    # Define a session with parameters.
    @nox.parametrize("param", ("a", "b", "c"))
    def my_session(session: nox.Session, param: str) -> None:
        pass

    func = Func(my_session, python=None)

    # Add the session to the manifest.
    for session in manifest.make_session("my_session", func):
        manifest.add_session(session)
    assert len(manifest) == 3


def test_add_session_parametrized_multiple_pythons() -> None:
    manifest = Manifest({}, create_mock_config())

    # Define a session with parameters.
    @nox.parametrize("param", ("a", "b"))
    def my_session(session: nox.Session, param: str) -> None:
        pass

    func = Func(my_session, python=["2.7", "3.6"])

    # Add the session to the manifest.
    for session in manifest.make_session("my_session", func):
        manifest.add_session(session)
    assert len(manifest) == 4


def test_add_session_parametrized_noop() -> None:
    manifest = Manifest({}, create_mock_config())

    # Define a session without any parameters.
    @nox.parametrize("param", ())
    def my_session(session: nox.Session, param: object) -> None:
        pass

    my_session.python = None
    my_session.venv_backend = None

    # Add the session to the manifest.
    for session in manifest.make_session("my_session", my_session):
        manifest.add_session(session)
    assert len(manifest) == 1

    session = manifest["my_session"]

    assert session.func == _null_session_func


def test_add_dependencies_with_empty_parametrize() -> None:
    manifest = Manifest({}, create_mock_config())

    # Define a session without any parameters available.
    @nox.parametrize("param", ())
    def empty(session: nox.Session, param: object) -> None:
        pass

    # Add the placeholder session and a regular session to the manifest.
    for session in manifest.make_session("empty", Func(empty, python=None)):
        manifest.add_session(session)
    for session in manifest.make_session("regular", Func(lambda _: None)):
        manifest.add_session(session)
    assert len(manifest) == 2

    # The placeholder session has no signatures, but dependency resolution
    # must still account for it.
    manifest.add_dependencies()

    assert [session.name for session in manifest._queue] == ["empty", "regular"]


def test_add_dependencies_with_empty_parametrize_and_multiple_pythons() -> None:
    manifest = Manifest({}, create_mock_config())

    # A session parametrized over multiple Pythons with an empty parameter list
    # expands to one placeholder (``multi=True``) session per interpreter, each
    # with no signatures. ``add_dependencies`` builds a fake parent session for
    # such parametrized names and must not assume every placeholder has a
    # signature.
    @nox.parametrize("param", ())
    def empty(session: nox.Session, param: object) -> None:
        pass

    for session in manifest.make_session("empty", Func(empty, python=["3.10", "3.11"])):
        manifest.add_session(session)

    # Must not raise IndexError.
    manifest.add_dependencies()

    assert [session.name for session in manifest._queue] == ["empty", "empty"]


def test_notify() -> None:
    manifest = Manifest({}, create_mock_config())

    # Define a session.
    def my_session_raw(session: nox.Session) -> None:
        pass

    my_session = typing.cast("Func", my_session_raw)

    my_session.python = None
    my_session.venv_backend = None

    def notified_raw(session: nox.Session) -> None:
        pass

    notified = typing.cast("Func", notified_raw)

    notified.python = None
    notified.venv_backend = None

    # Add the sessions to the manifest.
    for session in manifest.make_session("my_session", my_session):
        manifest.add_session(session)
    for session in manifest.make_session("notified", notified):
        manifest.add_session(session)
    assert len(manifest) == 2

    # Filter so only the first session is included in the queue.
    manifest.filter_by_name(("my_session",))
    assert len(manifest) == 1

    # Notify the notified session.
    manifest.notify("notified")
    assert len(manifest) == 2


def test_notify_noop() -> None:
    manifest = Manifest({}, create_mock_config())

    # Define a session and add it to the manifest.
    def my_session_raw(session: nox.Session) -> None:
        pass

    my_session = typing.cast("Func", my_session_raw)

    my_session.python = None
    my_session.venv_backend = None

    for session in manifest.make_session("my_session", my_session):
        manifest.add_session(session)

    assert len(manifest) == 1

    # Establish idempotency; notifying a session already in the queue no-ops.
    manifest.notify("my_session")
    assert len(manifest) == 1


def test_notify_with_posargs() -> None:
    cfg = create_mock_config()
    manifest = Manifest({}, cfg)

    session = manifest.make_session("my_session", Func(lambda _: None))[0]
    manifest.add_session(session)

    # delete my_session from the queue
    manifest.filter_by_name(())

    assert session.posargs == cfg.posargs
    assert manifest.notify("my_session", posargs=["--an-arg"])
    assert session.posargs == ["--an-arg"]


def test_notify_error() -> None:
    manifest = Manifest({}, create_mock_config())
    with pytest.raises(ValueError, match="Session does_not_exist not found"):
        manifest.notify("does_not_exist")


def test_add_session_idempotent() -> None:
    manifest = Manifest({}, create_mock_config())
    session_func = mock.Mock(spec=(), python=None, venv_backend=None)
    for session in manifest.make_session("my_session", session_func):
        manifest.add_session(session)
        manifest.add_session(session)
    assert len(manifest) == 1


def test_null_session_function() -> None:
    session = mock.Mock(spec=("skip",))
    _null_session_func(session)
    assert session.skip.called


def _make_variant_manifest() -> Manifest:
    """A manifest with one 2x2 ``prepare`` matrix and a ``check`` per cell."""

    @nox.session(venv_backend="none")
    @nox.parametrize("python", ["3.10", "3.11"])
    @nox.parametrize("kind", ["json", "yaml"])
    def prepare(session: nox.Session, kind: str) -> None:
        pass

    @nox.session(
        venv_backend="none", requires=[nox.depends_on("prepare", kind="yaml")]
    )
    @nox.parametrize("python", ["3.10", "3.11"])
    def check(session: nox.Session) -> None:
        pass

    return Manifest({"prepare": prepare, "check": check}, create_mock_config())


def test_resolve_dependency_plain_name_pulls_all_variants() -> None:
    manifest = _make_variant_manifest()
    # A plain name keeps the old rule: every variant of the parametrized session.
    resolved = manifest.resolve_dependency("prepare")
    assert [s.func.python for s in resolved] == ["3.10", "3.10", "3.11", "3.11"]
    assert [s.func.call_spec["kind"] for s in resolved] == [
        "json",
        "yaml",
        "json",
        "yaml",
    ]


def test_resolve_dependency_plain_signature() -> None:
    manifest = _make_variant_manifest()
    resolved = manifest.resolve_dependency("prepare(kind='yaml', python='3.10')")
    assert len(resolved) == 1
    assert resolved[0].func.python == "3.10"
    assert resolved[0].func.call_spec == {"kind": "yaml"}


def test_resolve_dependency_variant_selector_matches_one() -> None:
    manifest = _make_variant_manifest()
    selector = nox.depends_on("prepare", python="3.11", kind="yaml")
    resolved = manifest.resolve_dependency(selector)
    assert len(resolved) == 1
    assert resolved[0].func.python == "3.11"
    assert resolved[0].func.call_spec == {"kind": "yaml"}


def test_resolve_dependency_variant_selector_uses_requirer_python() -> None:
    manifest = _make_variant_manifest()
    requirer = manifest["check(python='3.10')"]
    resolved = manifest.resolve_dependency(
        nox.depends_on("prepare", kind="yaml"), requirer
    )
    assert len(resolved) == 1
    assert resolved[0].func.python == "3.10"
    assert resolved[0].func.call_spec == {"kind": "yaml"}


def test_resolve_dependency_variant_selector_ambiguous() -> None:
    manifest = _make_variant_manifest()
    with pytest.raises(DependencyVariantError) as excinfo:
        manifest.resolve_dependency(nox.depends_on("prepare", kind="yaml"))
    message = str(excinfo.value)
    assert "ambiguous" in message
    assert "Selectors: kind='yaml'" in message
    assert "Available variants:" in message


def test_resolve_dependency_variant_selector_no_match() -> None:
    manifest = _make_variant_manifest()
    with pytest.raises(DependencyVariantError) as excinfo:
        manifest.resolve_dependency(nox.depends_on("prepare", kind="toml"))
    message = str(excinfo.value)
    assert "matches no variant of prepare" in message
    assert "Available variants:" in message


def test_resolve_dependency_variant_selector_missing_session() -> None:
    manifest = _make_variant_manifest()
    with pytest.raises(KeyError, match="Session not found: does_not_exist"):
        manifest.resolve_dependency(nox.depends_on("does_not_exist"))


def test_resolve_dependency_single_variant_session() -> None:
    # A non-parametrized session is still selectable by a variant selector.
    manifest = Manifest({}, create_mock_config())

    def single(session: nox.Session) -> None:
        pass

    for session in manifest.make_session("single", Func(single, python=None)):
        manifest.add_session(session)

    resolved = manifest.resolve_dependency(nox.depends_on("single"))
    assert len(resolved) == 1


def test_resolve_dependency_variant_selector_empty_parametrize() -> None:
    # A session whose parameters were empty has only a signature-less
    # placeholder; a selector reports that rather than returning an
    # unreachable prerequisite.
    manifest = Manifest({}, create_mock_config())

    @nox.parametrize("kind", [])
    def empty(session: nox.Session, kind: str) -> None:
        pass

    for session in manifest.make_session("empty", Func(empty, python=None)):
        manifest.add_session(session)

    with pytest.raises(DependencyVariantError, match="no available variants"):
        manifest.resolve_dependency(nox.depends_on("empty"))


def test_resolve_dependency_variant_selector_python_as_param() -> None:
    # When the session function itself takes a ``python`` argument, its value
    # lives in the variant's call spec; a selector still matches on it.
    manifest = Manifest({}, create_mock_config())

    @nox.parametrize("python,dependency", [("3.10", "1.0"), ("3.11", "2.0")])
    def build(session: nox.Session, python: str, dependency: str) -> None:
        pass

    for session in manifest.make_session("build", Func(build, python=None)):
        manifest.add_session(session)

    resolved = manifest.resolve_dependency(
        nox.depends_on("build", python="3.10", dependency="1.0")
    )
    assert len(resolved) == 1
    assert resolved[0].func.call_spec == {"python": "3.10", "dependency": "1.0"}


def test_add_dependencies_with_variant_selector_only_one_cell() -> None:
    manifest = _make_variant_manifest()
    manifest.filter_by_name(("check(python='3.11')",))
    manifest.add_dependencies()
    # Only the selected ``check`` cell plus its own-interpreter ``prepare`` cell.
    assert [session.friendly_name for session in manifest] == [
        "prepare(kind='yaml', python='3.11')",
        "check(python='3.11')",
    ]


def test_keyword_locals_length() -> None:
    kw = KeywordLocals({"foo", "bar"})
    assert len(kw) == 2


def test_keyword_locals_iter() -> None:
    values = ["bar", "foo"]
    kw = KeywordLocals(values)
    assert sorted(kw) == values


def test_no_venv_backend_but_some_pythons() -> None:
    manifest = Manifest({}, create_mock_config())

    # Define a session and add it to the manifest.
    def my_session_raw(session: nox.Session) -> None:
        pass

    my_session = typing.cast("Func", my_session_raw)

    # the session sets "no venv backend" but declares some pythons
    my_session.python = ["3.7", "3.8"]
    my_session.venv_backend = "none"
    my_session.should_warn = {}

    sessions = manifest.make_session("my_session", my_session)

    # check that the pythons were correctly removed (a log warning is also emitted)
    assert sessions[0].func.python is False
    assert sessions[0].func.should_warn == {WARN_PYTHONS_IGNORED: ["3.7", "3.8"]}
