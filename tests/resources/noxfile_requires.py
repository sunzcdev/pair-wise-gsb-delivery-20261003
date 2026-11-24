# Copyright 2022 Alethea Katherine Flowers
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

import nox


@nox.session(requires=["c", "b"])
def a(session):
    print(session.name)


@nox.session()
def b(session):
    print(session.name)


@nox.session()
def c(session):
    print(session.name)


@nox.session(requires=["e"])
def d(session):
    print(session.name)


@nox.session(requires=["c"])
def e(session):
    print(session.name)


@nox.session(requires=["b", "g"])
def f(session):
    print(session.name)


@nox.session(requires=["b", "h"])
def g(session):
    print(session.name)


@nox.session(requires=["c"])
def h(session):
    print(session.name)


@nox.session(requires=["j"])
def i(session):
    print(session.name)


@nox.session(requires=["i"])
def j(session):
    print(session.name)


@nox.session(python=["3.10", "3.11"])
def k(session):
    print(session.name)


@nox.session(requires=["k"])
def m(session):
    print(session.name)


@nox.session(python="3.11", requires=["k-{python}"])
def n(session):
    print(session.name)


@nox.session(requires=["does_not_exist"])
def o(session):
    print(session.name)


@nox.session(python=["3.10", "3.11"])
def p(session):
    print(session.name)


@nox.session(python=None, requires=["p-{python}"])
def q(session):
    print(session.name)


@nox.session
def r(session):
    print(session.name)
    msg = "Fail!"
    raise Exception(msg)


@nox.session(requires=["r"])
def s(session):
    print(session.name)


@nox.session(requires=["r"])
def t(session):
    print(session.name)


@nox.parametrize("django", ["1.9", "2.0"])
@nox.session
def u(session, django):
    print(session.name)


@nox.session(requires=["u(django='1.9')", "u(django='2.0')"])
def v(session):
    print(session.name)


@nox.session(requires=["u"])
def w(session):
    print(session.name)


# A 2x2 matrix of interpreter and data format. ``yaml`` always succeeds; every
# ``json`` cell fails, so a selector that picks the wrong cell is visible.
@nox.session(venv_backend="none", default=False)
@nox.parametrize("python", ["3.10", "3.11"])
@nox.parametrize("kind", ["json", "yaml"])
def prepare(session, kind):
    print(f"prepare-{session.python}-{kind}")
    if kind == "json":
        msg = "unrelated failure"
        session.error(msg)


# Depends only on the ``prepare`` cell sharing its own interpreter, so the
# failing ``json`` cell (and the other interpreter's cell) never runs.
@nox.session(
    venv_backend="none", default=False, requires=[nox.depends_on("prepare", kind="yaml")]
)
@nox.parametrize("python", ["3.10", "3.11"])
def consume(session):
    print(f"consume-{session.python}")


# Explicitly pins both the interpreter and the format, regardless of its own.
@nox.session(
    venv_backend="none",
    default=False,
    requires=[nox.depends_on("prepare", python="3.11", kind="yaml")],
)
def lint(session):
    print("lint")


# A selector that matches more than one variant is rejected before anything runs.
@nox.session(venv_backend="none", default=False, requires=[nox.depends_on("prepare", kind="yaml")])
def ambiguous(session):
    print("ambiguous")


# A selector matching no variant is rejected before anything runs.
@nox.session(
    venv_backend="none", default=False, requires=[nox.depends_on("prepare", kind="toml")]
)
def missing_variant(session):
    print("missing_variant")


# A selector naming an unknown session is reported as a missing session.
@nox.session(
    venv_backend="none", default=False, requires=[nox.depends_on("does_not_exist")]
)
def missing_session(session):
    print("missing_session")
