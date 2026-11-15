"""Local-only pytest plugin (loaded via -p) providing tmpdir/tmp_path fixtures
backed by a plain workspace directory. The sandbox blocks the directory
lifecycle used by pytest's built-in tmpdir plugin on this Windows filesystem.
"""

import os
import uuid
from pathlib import Path

import pytest

BASE = Path(__file__).parent / 'localrun' / 'tmp'
BASE.mkdir(parents=True, exist_ok=True)


@pytest.fixture
def tmp_path_factory():
    class Factory:
        def mktemp(self, name=None):
            path = BASE / (name or uuid.uuid4().hex)
            path.mkdir(parents=True, exist_ok=True)
            return path

        def getbasetemp(self):
            return BASE

    return Factory()


@pytest.fixture
def tmp_path(tmp_path_factory):
    return tmp_path_factory.mktemp()


@pytest.fixture
def tmpdir(tmp_path):
    import py

    return py.path.local(str(tmp_path))
