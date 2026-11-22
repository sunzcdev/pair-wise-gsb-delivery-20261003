from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING, Final

import pytest

from virtualenv.config.cli.parser import VirtualEnvOptions
from virtualenv.config.ini import IniConfig
from virtualenv.run import audit_via_cli, session_via_cli

if TYPE_CHECKING:
    from collections.abc import Mapping


_NO_CONFIG: Final = {"VIRTUALENV_CONFIG_FILE": "missing.ini"}


def _has(lines: tuple[str, ...], text: str) -> bool:
    return any(line.startswith(text) for line in lines)


def _write_ini(tmp_path: Path, body: str) -> Path:
    config = tmp_path / "virtualenv.ini"
    config.write_text(f"[virtualenv]\n{body}\n", encoding="utf-8")
    return config


def _audit(tmp_path: Path, args: list[str] | None = None, ini: str = "", env: Mapping[str, str] | None = None):
    config = _write_ini(tmp_path, ini)
    return audit_via_cli(
        [str(tmp_path / "env"), *(args or [])],
        env={**_NO_CONFIG, "VIRTUALENV_CONFIG_FILE": str(config), **(env or {})},
    )


def test_flag_passed_on_the_command_line_is_reported_as_cli(tmp_path: Path) -> None:
    # the report exists for this case: a --no-periodic-update passed on the command line must not read as the default
    audit = _audit(tmp_path, ["--no-periodic-update"])

    assert (audit.value("no_periodic_update"), audit.source("no_periodic_update")) == (True, "cli")


def test_negative_flag_reads_as_the_behavior_it_turns_off(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ["--no-periodic-update"])

    assert _has(audit.summary(), "periodic update: disabled")


def test_flag_uses_the_config_file_when_the_command_line_is_silent(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ini="no_periodic_update = true")

    assert (audit.value("no_periodic_update"), audit.source("no_periodic_update")) == (True, "ini")
    assert _has(audit.summary(), "periodic update: disabled")


def test_false_in_the_config_file_leaves_a_negative_flag_off(tmp_path: Path) -> None:
    # the config file supplied the value, so the report names it, but false keeps the update running
    audit = _audit(tmp_path, ini="no_periodic_update = false")

    assert (audit.value("no_periodic_update"), audit.source("no_periodic_update")) == (False, "ini")
    assert _has(audit.summary(), "periodic update: enabled")
    assert audit.readings("no_periodic_update", "ini")[0].used is True


def test_empty_environment_variable_is_told_apart_from_an_unset_one(tmp_path: Path) -> None:
    empty = _audit(tmp_path, env={"VIRTUALENV_NO_PERIODIC_UPDATE": ""})
    unset = _audit(tmp_path)
    missing = _audit(tmp_path, env={"VIRTUALENV_NO_PERIODIC_UPDATE": "true"})

    assert empty.readings("no_periodic_update", "env var")[0].status == "empty"
    assert unset.readings("no_periodic_update", "env var")[0].status == "unset"
    assert (empty.source("no_periodic_update"), empty.value("no_periodic_update")) == ("default", False)
    assert (missing.source("no_periodic_update"), missing.value("no_periodic_update")) == ("env var", True)


def test_environment_variable_outranks_the_config_file(tmp_path: Path) -> None:
    audit = _audit(
        tmp_path,
        ini="creator = venv",
        env={"VIRTUALENV_CREATOR": "builtin"},
    )

    assert (audit.value("creator"), audit.source("creator")) == ("builtin", "env var")
    ini_reading = audit.readings("creator", "ini")[0]
    assert (ini_reading.status, ini_reading.raw, ini_reading.used) == ("set", "venv", False)
    assert any("outranks the config file" in note for note in audit.get("creator").notes)


def test_command_line_overrides_both_configured_layers(tmp_path: Path) -> None:
    audit = _audit(
        tmp_path,
        ["--no-periodic-update"],
        ini="no_periodic_update = false",
        env={"VIRTUALENV_NO_PERIODIC_UPDATE": "false"},
    )

    assert (audit.value("no_periodic_update"), audit.source("no_periodic_update")) == (True, "cli")
    readings = {reading.layer: reading for reading in audit.readings("no_periodic_update")}
    assert (readings["env var"].status, readings["env var"].used) == ("set", False)
    assert (readings["ini"].status, readings["ini"].used) == ("set", False)


def test_list_option_from_all_three_layers_names_the_override(tmp_path: Path) -> None:
    for name in ("from-ini", "from-env", "from-cli"):
        (tmp_path / name).mkdir()
    audit = _audit(
        tmp_path,
        ["--extra-search-dir", str(tmp_path / "from-cli")],
        ini=f"extra_search_dir = {tmp_path / 'from-ini'}",
        env={"VIRTUALENV_EXTRA_SEARCH_DIR": str(tmp_path / "from-env")},
    )

    option = audit.get("extra_search_dir")
    assert option.kind == "list"
    assert (option.value, option.source) == ([tmp_path / "from-cli"], "cli")
    assert {reading.layer: reading.status for reading in option.readings} == {"env var": "set", "ini": "set"}
    assert not any(reading.used for reading in option.readings)


def test_list_option_comes_from_the_config_file_when_the_command_line_is_silent(tmp_path: Path) -> None:
    (tmp_path / "from-ini").mkdir()
    configured = tmp_path / "from-ini"
    audit = _audit(tmp_path, ini=f"extra_search_dir = {configured}")

    assert (audit.value("extra_search_dir"), audit.source("extra_search_dir")) == ([configured], "ini")
    assert audit.readings("extra_search_dir", "ini")[0].used is True


def test_invalid_environment_value_names_the_variable_and_its_problem(tmp_path: Path) -> None:
    audit = _audit(tmp_path, env={"VIRTUALENV_NO_PERIODIC_UPDATE": "maybe"})

    reading = audit.readings("no_periodic_update", "env var")[0]
    assert (reading.status, reading.raw, reading.problem) == ("invalid", "maybe", "Not a boolean: maybe")
    assert any("VIRTUALENV_NO_PERIODIC_UPDATE" in note for note in audit.get("no_periodic_update").notes)


def test_invalid_config_value_names_the_key_and_its_problem(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ini="no_periodic_update = maybe")

    reading = audit.readings("no_periodic_update", "ini")[0]
    assert (reading.status, reading.raw, reading.problem) == ("invalid", "maybe", "Not a boolean: maybe")
    assert (audit.value("no_periodic_update"), audit.source("no_periodic_update")) == (False, "default")
    notes = audit.get("no_periodic_update").notes
    assert any("no_periodic_update" in note and "cannot be parsed" in note for note in notes)


def test_invalid_environment_value_falls_through_to_a_valid_config_value(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ini="verbose = 4", env={"VIRTUALENV_VERBOSE": "not-a-number"})

    assert (audit.value("verbose"), audit.source("verbose")) == (4, "ini")
    statuses = {reading.name: reading.status for reading in audit.readings("verbose", "env var")}
    assert statuses == {"VIRTUALENV_V": "unset", "VIRTUALENV_VERBOSE": "invalid"}


def test_historical_hyphenated_config_key_is_read_and_labelled(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ini="no-periodic-update = true")

    reading = audit.readings("no_periodic_update", "ini")[0]
    assert (reading.name, reading.status, reading.legacy) == ("no-periodic-update", "set", True)
    assert (audit.value("no_periodic_update"), audit.source("no_periodic_update")) == (True, "ini")
    assert any("historical hyphenated spelling" in note for note in audit.get("no_periodic_update").notes)


def test_hyphenated_key_still_reaches_the_creation(tmp_path: Path) -> None:
    config = _write_ini(tmp_path, "no-periodic-update = true")

    session = session_via_cli([str(tmp_path / "env")], env={**_NO_CONFIG, "VIRTUALENV_CONFIG_FILE": str(config)})

    assert session.seeder.periodic_update is False


def test_missing_config_file_is_reported_without_failing(tmp_path: Path) -> None:
    env = {**_NO_CONFIG, "VIRTUALENV_CONFIG_FILE": str(tmp_path / "no.ini")}
    audit = audit_via_cli([str(tmp_path / "env")], env=env)

    assert audit.config_state == "missing"
    assert audit.source("no_periodic_update") == "default"


def test_report_matches_the_seeder_the_creation_uses(tmp_path: Path) -> None:
    # the report is built from the same parse the creation reads, so the two cannot drift
    config = _write_ini(tmp_path, "no_periodic_update = true\nextra_search_dir =\n")
    env = {**_NO_CONFIG, "VIRTUALENV_CONFIG_FILE": str(config)}
    options = VirtualEnvOptions()

    session = session_via_cli([str(tmp_path / "env")], options=options, env=env)
    audit = options and session.config_audit

    assert audit is not None
    assert audit.value("no_periodic_update") is options.no_periodic_update
    assert (options.no_periodic_update, session.seeder.periodic_update) == (True, False)


def test_options_object_receives_the_audit_values(tmp_path: Path) -> None:
    options = VirtualEnvOptions()
    env = {**_NO_CONFIG, "VIRTUALENV_NO_PERIODIC_UPDATE": "1"}

    session = session_via_cli([str(tmp_path / "env")], options=options, env=env)

    assert session.config_audit.value("no_periodic_update") is options.no_periodic_update is True
    assert session.seeder.periodic_update is False


def test_every_option_the_creation_reads_is_in_the_report(tmp_path: Path) -> None:
    audit = _audit(tmp_path)

    for dest in ("dest", "creator", "seeder", "activators", "no_periodic_update", "download", "extra_search_dir"):
        assert audit.get(dest) is not None, dest


def test_report_names_the_layer_of_each_value(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ["--no-periodic-update"])
    text = audit.render(["no_periodic_update"])

    assert "no_periodic_update" in text
    assert "from cli" in text
    assert "cli: passed" in text


def test_audit_cli_flag_exits_without_creating(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    from virtualenv.run import cli_run  # ruff:ignore[import-outside-top-level]

    env = {**_NO_CONFIG, "VIRTUALENV_CONFIG_FILE": str(_write_ini(tmp_path, "no_periodic_update = true"))}
    with pytest.raises(SystemExit) as context:
        cli_run([str(tmp_path / "env"), "--config-audit"], env=env)

    assert context.value.code == 0
    assert not (tmp_path / "env").exists()
    out = capsys.readouterr().out
    assert "config file" in out
    assert "periodic update: disabled" in out


def test_ini_reading_is_cached_per_key_and_converter(tmp_path: Path) -> None:
    from virtualenv.config.convert import BoolType  # ruff:ignore[import-outside-top-level]

    config = IniConfig(env={"VIRTUALENV_CONFIG_FILE": str(_write_ini(tmp_path, "no_periodic_update = true"))})

    as_type = BoolType(bool, bool)
    first = config.read("no_periodic_update", as_type)
    second = config.read("no_periodic_update", as_type)

    assert first is second
    assert (first.status, first.value) == ("set", True)


def test_ini_that_is_not_active_reports_a_skipped_layer(tmp_path: Path) -> None:
    env = {**_NO_CONFIG, "VIRTUALENV_CONFIG_FILE": str(tmp_path / "no.ini")}
    audit = audit_via_cli([str(tmp_path / "env")], env=env)

    assert audit.readings("no_periodic_update", "ini")[0].status == "skipped"


def test_three_layers_set_different_values_and_the_report_names_the_winner(tmp_path: Path) -> None:
    for name in ("from-ini", "from-env", "from-cli"):
        (tmp_path / name).mkdir()
    env = {"VIRTUALENV_EXTRA_SEARCH_DIR": str(tmp_path / "from-env"), "VIRTUALENV_NO_PERIODIC_UPDATE": "1"}

    audit = _audit(
        tmp_path,
        ["--extra-search-dir", str(tmp_path / "from-cli"), "--no-periodic-update"],
        ini=f"extra_search_dir = {tmp_path / 'from-ini'}\nno_periodic_update = false",
        env=env,
    )

    # each layer holds a different value, and the command line is what the creation ends up using
    assert audit.value("extra_search_dir") == [tmp_path / "from-cli"]
    readings = {reading.layer: reading for reading in audit.readings("extra_search_dir")}
    assert {layer: reading.raw for layer, reading in readings.items()} == {
        "env var": str(tmp_path / "from-env"),
        "ini": str(tmp_path / "from-ini"),
    }
    assert (audit.value("no_periodic_update"), audit.source("no_periodic_update")) == (True, "cli")


def test_report_and_seeder_agree_on_a_value_set_at_every_layer(tmp_path: Path) -> None:
    # the task's acceptance: a report of a three-layer run must match the options the creation actually used
    config = _write_ini(tmp_path, "no_periodic_update = false\ndownload = false")
    env = {
        **_NO_CONFIG,
        "VIRTUALENV_CONFIG_FILE": str(config),
        "VIRTUALENV_NO_PERIODIC_UPDATE": "false",
        "VIRTUALENV_DOWNLOAD": "true",
    }
    options = VirtualEnvOptions()

    session = session_via_cli(
        [str(tmp_path / "env"), "--no-periodic-update", "--no-download"], options=options, env=env
    )

    audit = session.config_audit
    assert audit is not None
    assert (audit.value("no_periodic_update"), session.seeder.periodic_update) == (True, False)
    assert (audit.value("download"), bool(session.seeder.download)) == (False, False)
    assert options.no_periodic_update is audit.value("no_periodic_update")
    assert options.download is audit.value("download")


def test_count_option_adds_the_command_line_on_top_of_the_configured_number(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ["-v"], ini="verbose = 4", env={"VIRTUALENV_VERBOSE": "3"})

    # the count the creation reads is the configured number plus one, exactly as argparse counts it
    assert (audit.value("verbose"), audit.source("verbose")) == (4, "cli")
    assert audit.get("verbose").kind == "count"
    assert any("adds to the env var count" in note for note in audit.get("verbose").notes)


def test_option_lookup_accepts_the_command_line_spelling(tmp_path: Path) -> None:
    audit = _audit(tmp_path, ["--no-periodic-update"])

    assert audit.get("--no-periodic-update") is audit.get("no_periodic_update")




