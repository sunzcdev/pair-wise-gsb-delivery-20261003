from __future__ import annotations

from typing import Final

import pytest

from virtualenv.config.convert import BoolType, ListType
from virtualenv.config.env_var import read_env_var
from virtualenv.config.ini import IniConfig

_BOOL: Final = BoolType(bool, bool)


@pytest.mark.parametrize(
    ("env", "status", "value"),
    [
        pytest.param({}, "unset", None, id="unset"),
        pytest.param({"VIRTUALENV_NO_PERIODIC_UPDATE": ""}, "empty", None, id="empty"),
        pytest.param({"VIRTUALENV_NO_PERIODIC_UPDATE": "yes"}, "set", True, id="set"),
        pytest.param({"VIRTUALENV_NO_PERIODIC_UPDATE": "maybe"}, "invalid", None, id="invalid"),
    ],
)
def test_read_env_var_reports_the_layer_status(env, status, value) -> None:
    result = read_env_var("no_periodic_update", _BOOL, env)

    assert (result.name, result.status, result.value) == ("VIRTUALENV_NO_PERIODIC_UPDATE", status, value)


def test_read_env_var_names_the_parse_problem() -> None:
    result = read_env_var("no_periodic_update", _BOOL, {"VIRTUALENV_NO_PERIODIC_UPDATE": "maybe"})

    assert (result.raw, result.problem) == ("maybe", "Not a boolean: maybe")


def test_ini_value_on_separate_lines_is_read(tmp_path) -> None:
    config_file = tmp_path / "conf.ini"
    config_file.write_text("[virtualenv]\nextra_search_dir =\n    one\n    two\n", encoding="utf-8")
    config = IniConfig(env={"VIRTUALENV_CONFIG_FILE": str(config_file)})

    result = config.read("extra_search_dir", ListType(list, str))

    assert (result.status, result.value) == ("set", ["one", "two"])

