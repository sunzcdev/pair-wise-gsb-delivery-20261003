"""Tests for the optional pure-Python reader parse explanations."""

from io import BytesIO, StringIO

import pytest

import fastavro
from fastavro.read import (
    ExplanationEvent,
    FIELD_ALIAS,
    FIELD_DEFAULT,
    UNION_BRANCH,
    PROMOTION,
)

from .conftest import is_testing_cython_modules

pytestmark = pytest.mark.skipif(
    is_testing_cython_modules(),
    reason="parse explanations are only implemented for the pure-Python reader",
)


def _write(records, writer_schema, **kwargs):
    fo = BytesIO()
    fastavro.writer(fo, writer_schema, records, **kwargs)
    fo.seek(0)
    return fo


def _events(fo, reader_schema=None, **kwargs):
    events = []
    callback = kwargs.pop("explain_callback", events.append)
    avro_reader = fastavro.reader(
        fo, reader_schema, explain_callback=callback, **kwargs
    )
    records = list(avro_reader)
    return records, events


def test_explanation_event_is_a_dataclass_without_values():
    event = ExplanationEvent(
        UNION_BRANCH,
        record_index=3,
        path=["Rec", "u"],
        details={"writer_index": 1, "writer_branch": "int", "reader_branch": None},
    )
    assert event.event_type == UNION_BRANCH
    assert event.record_index == 3
    assert event.path == ["Rec", "u"]
    assert event.details == {
        "writer_index": 1,
        "writer_branch": "int",
        "reader_branch": None,
    }
    # Events never carry decoded field values.
    assert "value" not in event.details


def test_no_callback_collects_nothing():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int"]}],
    }
    fo = _write([{"u": ("int", 1)}], schema)
    avro_reader = fastavro.reader(fo)
    assert "_explain_collector" not in avro_reader.options
    assert list(avro_reader) == [{"u": 1}]


def test_union_branch_event_only_for_selected_branch():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int", "string"]}],
    }
    fo = _write([{"u": ("int", 5)}, {"u": None}], schema)
    records, events = _events(fo)

    assert records == [{"u": 5}, {"u": None}]
    branch_events = [e for e in events if e.event_type == UNION_BRANCH]
    assert len(branch_events) == 2
    assert branch_events[0].details == {
        "writer_index": 1,
        "writer_branch": "int",
        "reader_branch": None,
    }
    assert branch_events[1].details == {
        "writer_index": 0,
        "writer_branch": "null",
        "reader_branch": None,
    }
    # The unselected "string" branch is never reported as a success.
    assert all(e.details["writer_branch"] != "string" for e in branch_events)


def test_union_branch_with_reader_union_reports_reader_branch():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int"]}],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "double"]}],
    }
    fo = _write([{"u": ("int", 5)}], writer_schema)
    records, events = _events(fo, reader_schema)

    assert records == [{"u": 5.0}]
    branch = next(e for e in events if e.event_type == UNION_BRANCH)
    assert branch.details == {
        "writer_index": 1,
        "writer_branch": "int",
        "reader_branch": "double",
    }


def test_named_union_branch_uses_branch_name():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {
                "name": "u",
                "type": [
                    "null",
                    {
                        "type": "record",
                        "name": "Inner",
                        "fields": [{"name": "v", "type": "int"}],
                    },
                ],
            }
        ],
    }
    fo = _write([{"u": ("Inner", {"v": 1})}], schema)
    _, events = _events(fo)
    branch = next(e for e in events if e.event_type == UNION_BRANCH)
    assert branch.details["writer_branch"] == "Inner"
    # No reader schema was supplied, so no reader branch is reported.
    assert branch.details["reader_branch"] is None


def test_field_alias_event():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "old", "type": "int"}],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "new", "type": "int", "aliases": ["old"]},
        ],
    }
    fo = _write([{"old": 1}], writer_schema)
    records, events = _events(fo, reader_schema)

    assert records == [{"new": 1}]
    alias_events = [e for e in events if e.event_type == FIELD_ALIAS]
    assert len(alias_events) == 1
    assert alias_events[0].details == {
        "writer_field": "old",
        "reader_field": "new",
        "alias": "old",
    }
    assert alias_events[0].path == ["R"]


def test_direct_name_match_emits_no_alias_event():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "same", "type": "int"}],
    }
    fo = _write([{"same": 1}], schema)
    _, events = _events(fo, schema)
    assert all(e.event_type != FIELD_ALIAS for e in events)


def test_skipped_writer_field_emits_no_event():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "kept", "type": "int"},
            {"name": "dropped", "type": "string"},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "kept", "type": "int"}],
    }
    fo = _write([{"kept": 1, "dropped": "gone"}], writer_schema)
    records, events = _events(fo, reader_schema)

    assert records == [{"kept": 1}]
    # The skipped field must not be fabricated as a successful conversion.
    assert all("dropped" not in e.path for e in events)
    assert all(
        e.details.get("field") != "dropped"
        and e.details.get("writer_field") != "dropped"
        for e in events
    )


def test_default_event_only_when_default_is_used():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "kept", "type": "int"}],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "kept", "type": "int"},
            {"name": "added", "type": "int", "default": 42},
        ],
    }
    fo = _write([{"kept": 1}], writer_schema)
    records, events = _events(fo, reader_schema)

    assert records == [{"kept": 1, "added": 42}]
    default_events = [e for e in events if e.event_type == FIELD_DEFAULT]
    assert len(default_events) == 1
    assert default_events[0].details == {"field": "added"}
    assert default_events[0].path == ["R"]
    # The default value itself is never included.
    assert 42 not in default_events[0].details.values()


def test_promotion_event_only_for_actual_promotions():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "a", "type": "int"},
            {"name": "b", "type": "int"},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "a", "type": "double"},
            {"name": "b", "type": "long"},
        ],
    }
    fo = _write([{"a": 1, "b": 2}], writer_schema)
    records, events = _events(fo, reader_schema)

    assert records == [{"a": 1.0, "b": 2}]
    promotions = [e for e in events if e.event_type == PROMOTION]
    # int->double promotes; int->long is a no-op in Python and must not emit.
    assert len(promotions) == 1
    assert promotions[0].details == {"writer_type": "int", "reader_type": "double"}
    assert promotions[0].path == ["R", "a"]


def test_record_index_and_paths_for_multiple_records():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int"]}],
    }
    fo = _write([{"u": ("int", 1)}, {"u": None}, {"u": ("int", 3)}], schema)
    records, events = _events(fo)

    assert [r for r in records] == [{"u": 1}, {"u": None}, {"u": 3}]
    assert [e.record_index for e in events] == [0, 1, 2]
    assert all(e.path == ["R", "u"] for e in events)


def test_nested_record_paths():
    schema = {
        "type": "record",
        "name": "Outer",
        "fields": [
            {
                "name": "inner",
                "type": {
                    "type": "record",
                    "name": "Inner",
                    "fields": [{"name": "u", "type": ["null", "int"]}],
                },
            }
        ],
    }
    fo = _write(
        [
            {"inner": {"u": ("int", 7)}},
            {"inner": {"u": ("int", 8)}},
        ],
        schema,
    )
    _, events = _events(fo)
    branches = [e for e in events if e.event_type == UNION_BRANCH]
    assert len(branches) == 2
    expected_path = ["Outer", "inner", "Inner", "u"]
    assert all(branch.path == expected_path for branch in branches)
    # Each event gets its own path list copy, so mutating one cannot leak
    # into any other event.
    branches[0].path.append("mutated")
    assert branches[1].path == expected_path


def test_schemaless_and_container_paths_match():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "u", "type": ["null", "int"]},
            {"name": "x", "type": "int"},
        ],
    }
    record = {"u": ("int", 5), "x": 1}
    # Without a reader schema the container reader unwraps union tuples, so
    # compare against the same plain read the schemaless entry performs.
    expected_read = {"u": 5, "x": 1}

    container_fo = _write([record], schema)
    container_records, container_events = _events(container_fo)
    assert container_records == [expected_read]

    schemaless_fo = BytesIO()
    fastavro.schemaless_writer(schemaless_fo, schema, record)
    schemaless_fo.seek(0)
    schemaless_events = []
    result = fastavro.schemaless_reader(
        schemaless_fo, schema, explain_callback=schemaless_events.append
    )
    assert result == expected_read

    # Identical path semantics; schemaless reads a single record (index 0).
    assert [(e.event_type, e.path, e.details) for e in container_events] == [
        (e.event_type, e.path, e.details) for e in schemaless_events
    ]
    assert all(e.record_index == 0 for e in schemaless_events)


def test_block_reader_index_continuous_across_blocks():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int"]}],
    }
    records = [{"u": ("int", i)} for i in range(100)]
    fo = _write(records, schema, sync_interval=100)

    events = []
    block_count = 0
    for block in fastavro.block_reader(fo, explain_callback=events.append):
        block_count += 1
        list(block)
    assert block_count > 1
    assert [e.record_index for e in events] == list(range(100))


def test_callback_error_raises_by_default_and_does_not_fabricate_data():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int"]}],
    }
    fo = _write([{"u": ("int", 1)}, {"u": None}], schema)

    def boom(event):
        raise RuntimeError("callback failure")

    avro_reader = fastavro.reader(fo, explain_callback=boom)
    with pytest.raises(RuntimeError, match="callback failure"):
        list(avro_reader)


def test_callback_error_ignore_keeps_data_unchanged():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int"]}],
    }
    expected = [{"u": 1}, {"u": None}]
    fo = _write(expected, schema)

    def boom(event):
        raise RuntimeError("callback failure")

    avro_reader = fastavro.reader(fo, explain_callback=boom, on_explain_error="ignore")
    # Reading continues and the decoded data is exactly the normal output.
    assert list(avro_reader) == expected


def test_invalid_explain_options():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "x", "type": "int"}],
    }
    fo = _write([{"x": 1}], schema)

    with pytest.raises(TypeError):
        fastavro.reader(fo, explain_callback=123)

    with pytest.raises(ValueError):
        fastavro.reader(
            fo,
            explain_callback=lambda event: None,
            on_explain_error="swallow",
        )

    schemaless_fo = BytesIO()
    fastavro.schemaless_writer(schemaless_fo, schema, {"x": 1})
    schemaless_fo.seek(0)
    with pytest.raises(ValueError):
        fastavro.schemaless_reader(
            schemaless_fo,
            schema,
            explain_callback=lambda event: None,
            on_explain_error="nope",
        )


def test_schemaless_callback_error_policies():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "u", "type": ["null", "int"]}],
    }

    def boom(event):
        raise RuntimeError("callback failure")

    fo = BytesIO()
    fastavro.schemaless_writer(fo, schema, {"u": ("int", 1)})
    fo.seek(0)
    with pytest.raises(RuntimeError):
        fastavro.schemaless_reader(fo, schema, explain_callback=boom)

    fo.seek(0)
    assert fastavro.schemaless_reader(
        fo, schema, explain_callback=boom, on_explain_error="ignore"
    ) == {"u": 1}


def test_events_never_contain_field_values():
    writer_schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "secret", "type": "string"},
            {"name": "u", "type": ["null", "string"]},
        ],
    }
    reader_schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "renamed", "type": "string", "aliases": ["secret"]},
            {"name": "u", "type": ["null", "string"]},
            {"name": "d", "type": "string", "default": "hidden-default"},
        ],
    }
    fo = _write(
        [{"secret": "topsecret", "u": ("string", "unionsecret")}],
        writer_schema,
    )
    _, events = _events(fo, reader_schema)
    assert events
    for event in events:
        assert "topsecret" not in str(event)
        assert "unionsecret" not in str(event)
        assert "hidden-default" not in str(event)


def test_json_reader_uses_same_path_semantics():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [
            {"name": "u", "type": ["null", "int"]},
            {"name": "x", "type": "int"},
        ],
    }
    records = [{"u": ("int", 1), "x": 5}, {"u": None, "x": 6}]
    text_fo = StringIO()
    fastavro.json_writer(text_fo, schema, records)
    text_fo.seek(0)

    events = []
    result = list(fastavro.json_reader(text_fo, schema, explain_callback=events.append))
    assert result == [{"u": 1, "x": 5}, {"u": None, "x": 6}]
    branches = [e for e in events if e.event_type == UNION_BRANCH]
    assert [e.record_index for e in branches] == [0, 1]
    assert all(e.path == ["R", "u"] for e in branches)


def test_json_reader_validates_explain_options():
    schema = {
        "type": "record",
        "name": "R",
        "fields": [{"name": "x", "type": "int"}],
    }
    with pytest.raises(ValueError):
        fastavro.json_reader(
            StringIO(),
            schema,
            explain_callback=lambda event: None,
            on_explain_error="bogus",
        )


def test_recursive_schema_paths():
    schema = {
        "type": "record",
        "name": "Node",
        "fields": [
            {"name": "value", "type": "int"},
            {"name": "next", "type": ["null", "Node"]},
        ],
    }
    record = {
        "value": 1,
        "next": (
            "Node",
            {"value": 2, "next": ("Node", {"value": 3, "next": None})},
        ),
    }
    fo = BytesIO()
    fastavro.schemaless_writer(fo, schema, record)
    fo.seek(0)

    events = []
    result = fastavro.schemaless_reader(fo, schema, explain_callback=events.append)
    assert result == {
        "value": 1,
        "next": {"value": 2, "next": {"value": 3, "next": None}},
    }
    branches = [e for e in events if e.event_type == UNION_BRANCH]
    assert [e.path for e in branches] == [
        ["Node", "next"],
        ["Node", "next", "Node", "next"],
        ["Node", "next", "Node", "next", "Node", "next"],
    ]
    # The terminal null selection is the only branch reported at the deepest
    # level; the unselected "Node" branch there is never fabricated.
    assert branches[-1].details["writer_branch"] == "null"
