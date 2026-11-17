"""
Tests for the jsonlines library.
"""

import codecs
import collections
import io
import json
import os
import tempfile
from typing import Any, Iterable, List, Union

import attr
import jsonlines
import pytest


SAMPLE_BYTES = b'{"a": 1}\n{"b": 2}\n'
SAMPLE_TEXT = SAMPLE_BYTES.decode("utf-8")


def is_json_decode_error(exc: object) -> bool:
    if type(exc).__module__ == "ujson":
        # The ujson package has its own ujson.JSONDecodeError; because of the
        # line above this function also works if it's not installed.
        import ujson

        return isinstance(exc, ujson.JSONDecodeError)
    else:
        # Otherwise, this should be a stdlib json.JSONDecodeError, which also
        # works for orjson since orjson.JSONDecodeError inherits from it.
        return isinstance(exc, json.JSONDecodeError)


def test_reader() -> None:
    fp = io.BytesIO(SAMPLE_BYTES)
    with jsonlines.Reader(fp) as reader:
        it = iter(reader)
        assert next(it) == {"a": 1}
        assert next(it) == {"b": 2}
        with pytest.raises(StopIteration):
            next(it)
        with pytest.raises(EOFError):
            reader.read()


def test_reading_from_iterable() -> None:
    with jsonlines.Reader(["1", b"{}"]) as reader:
        assert list(reader) == [1, {}]
    assert "wrapping <list at " in repr(reader)


def test_reader_rfc7464_text_sequences() -> None:
    fp = io.BytesIO(b'\x1e"a"\x0a\x1e"b"\x0a')
    with jsonlines.Reader(fp) as reader:
        assert list(reader) == ["a", "b"]


def test_reader_utf8_bom_bytes() -> None:
    """
    UTF-8 BOM is ignored, even if it occurs in the middle of a stream.
    """
    chunks = [
        codecs.BOM_UTF8,
        b"1\n",
        codecs.BOM_UTF8,
        b"2\n",
    ]
    fp = io.BytesIO(b"".join(chunks))
    with jsonlines.Reader(fp) as reader:
        assert list(reader) == [1, 2]


def test_reader_utf8_bom_text() -> None:
    """
    Text version of ``test_reader_utf8_bom_bytes()``.
    """
    chunks = [
        "1\n",
        codecs.BOM_UTF8.decode(),
        "2\n",
    ]
    fp = io.StringIO("".join(chunks))
    with jsonlines.Reader(fp) as reader:
        assert list(reader) == [1, 2]


def test_reader_utf8_bom_bom_bom() -> None:
    """
    Too many UTF-8 BOM BOM BOM chars cause BOOM 💥 BOOM.
    """
    reader = jsonlines.Reader([codecs.BOM_UTF8.decode() * 3 + "1\n"])
    with pytest.raises(jsonlines.InvalidLineError) as excinfo:
        reader.read()

    exc = excinfo.value
    assert "invalid json" in str(exc)
    assert is_json_decode_error(exc.__cause__)


def test_writer_text() -> None:
    fp = io.StringIO()
    with jsonlines.Writer(fp) as writer:
        writer.write({"a": 1})
        writer.write({"b": 2})
    assert fp.getvalue() == SAMPLE_TEXT


def test_writer_binary() -> None:
    fp = io.BytesIO()
    with jsonlines.Writer(fp) as writer:
        writer.write_all(
            [
                {"a": 1},
                {"b": 2},
            ]
        )
    assert fp.getvalue() == SAMPLE_BYTES


def test_closing() -> None:
    reader = jsonlines.Reader([])
    reader.close()
    with pytest.raises(RuntimeError):
        reader.read()
    writer = jsonlines.Writer(io.BytesIO())
    writer.close()
    writer.close()  # no-op
    with pytest.raises(RuntimeError):
        writer.write(123)


def test_invalid_lines() -> None:
    data = "[1, 2"
    with jsonlines.Reader(io.StringIO(data)) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        exc = excinfo.value
        assert "invalid json" in str(exc)
        assert exc.line == data
        assert is_json_decode_error(exc.__cause__)


def test_skip_invalid() -> None:
    fp = io.StringIO("12\ninvalid\n34")
    reader = jsonlines.Reader(fp)
    it = reader.iter(skip_invalid=True)
    assert next(it) == 12
    assert next(it) == 34


def test_empty_strings_in_iterable() -> None:
    input = ["123", "", "456"]
    it = iter(jsonlines.Reader(input))
    assert next(it) == 123
    with pytest.raises(jsonlines.InvalidLineError):
        next(it)
    with pytest.raises(StopIteration):
        next(it)
    it = jsonlines.Reader(input).iter(skip_empty=True)
    assert list(it) == [123, 456]


def test_invalid_utf8() -> None:
    with jsonlines.Reader([b"\xff\xff"]) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        assert "line is not valid utf-8" in str(excinfo.value)


def test_empty_lines() -> None:
    data_with_empty_line = b"1\n\n2\n"
    with jsonlines.Reader(io.BytesIO(data_with_empty_line)) as reader:
        assert reader.read()
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read()
        assert reader.read() == 2
        with pytest.raises(EOFError):
            reader.read()
    with jsonlines.Reader(io.BytesIO(data_with_empty_line)) as reader:
        assert list(reader.iter(skip_empty=True)) == [1, 2]


def test_typed_reads() -> None:
    with jsonlines.Reader(io.StringIO('12\ntrue\n"foo"\n')) as reader:
        assert reader.read(type=int) == 12

        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read(type=int)
        exc = excinfo.value
        assert "does not match requested type" in str(exc)
        assert exc.line == "true"

        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read(type=float)
        exc = excinfo.value
        assert "does not match requested type" in str(exc)
        assert exc.line == '"foo"'


def test_typed_read_invalid_type() -> None:
    reader = jsonlines.Reader([])
    with pytest.raises(ValueError) as excinfo:
        reader.read(type="nope")  # type: ignore[call-overload]
    exc = excinfo.value
    assert str(exc) == "invalid type specified"


def test_typed_iteration() -> None:
    fp = io.StringIO("1\n2\n")
    with jsonlines.Reader(fp) as reader:
        actual = list(reader.iter(type=int))
        assert actual == [1, 2]

    fp = io.StringIO("1\n2\n")
    with jsonlines.Reader(fp) as reader:
        it = reader.iter(type=str)
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            next(it)
        exc = excinfo.value
        assert "does not match requested type" in str(exc)


def test_writer_flags() -> None:
    fp = io.BytesIO()
    with jsonlines.Writer(fp, compact=True, sort_keys=True) as writer:
        writer.write(
            collections.OrderedDict(
                [
                    ("b", 2),
                    ("a", 1),
                ]
            )
        )
    assert fp.getvalue() == b'{"a":1,"b":2}\n'


def test_custom_dumps() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, dumps=lambda obj: "oh hai")
    with writer:
        nbytes = writer.write({})
        assert nbytes == len(b"oh hai\n")

    assert fp.getvalue() == b"oh hai\n"


def test_custom_dumps_bytes() -> None:
    """
    A custom dump function that returns bytes (e.g. ‘orjson’) should work.
    """

    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, dumps=lambda obj: b"some bytes")
    with writer:
        writer.write(123)

    assert fp.getvalue() == b"some bytes\n"


def test_custom_loads() -> None:
    fp = io.BytesIO(b"{}\n")
    with jsonlines.Reader(fp, loads=lambda s: "uh what") as reader:
        assert reader.read() == "uh what"


def test_open_reading() -> None:
    with tempfile.NamedTemporaryFile("wb") as fp:
        fp.write(b"123\n")
        fp.flush()
        with jsonlines.open(fp.name) as reader:
            assert list(reader) == [123]


def test_open_reading_with_utf8_bom() -> None:
    """
    The ``.open()`` helper ignores a UTF-8 BOM.
    """
    with tempfile.NamedTemporaryFile("wb") as fp:
        fp.write(codecs.BOM_UTF8)
        fp.write(b"123\n")
        fp.flush()
        with jsonlines.open(fp.name) as reader:
            assert list(reader) == [123]


def test_open_writing() -> None:
    with tempfile.NamedTemporaryFile("w+b") as fp:
        with jsonlines.open(fp.name, mode="w") as writer:
            writer.write(123)
        assert fp.read() == b"123\n"
    assert fp.name in repr(writer)


def test_open_and_append_writing() -> None:
    with tempfile.NamedTemporaryFile("w+b") as fp:
        with jsonlines.open(fp.name, mode="w") as writer:
            nbytes = writer.write(123)
            assert nbytes == len(str(123)) + 1
        with jsonlines.open(fp.name, mode="a") as writer:
            nbytes = writer.write(456)
            assert nbytes == len(str(456)) + 1
        assert fp.read() == b"123\n456\n"


def test_open_invalid_mode() -> None:
    with pytest.raises(ValueError) as excinfo:
        jsonlines.open("foo", mode="foo")
    assert "mode" in str(excinfo.value)


def test_single_char_stripping() -> None:
    """ "
    Sanity check that a helper constant actually contains single-char strings.
    """
    assert all(len(s) == 1 for s in jsonlines.jsonlines.SKIPPABLE_SINGLE_INITIAL_CHARS)


# ---------------------------------------------------------------------------
# Codec configuration snapshots
# ---------------------------------------------------------------------------


INSTALLED_BACKENDS: List[jsonlines.Backend] = [
    backend
    for backend in (
        jsonlines.Backend.JSON,
        jsonlines.Backend.ORJSON,
        jsonlines.Backend.UJSON,
    )
    if getattr(jsonlines.jsonlines, backend.value, None) is not None
]


@pytest.fixture(autouse=True)
def reset_default_backend() -> Iterable[None]:
    jsonlines.set_default_backend(None)
    yield
    jsonlines.set_default_backend(None)


class UntouchableSource:
    """
    Iterable that fails loudly if anything iterates over it.
    """

    def __init__(self) -> None:
        self.touched = False

    def __iter__(self) -> "UntouchableSource":
        self.touched = True
        raise AssertionError("source must not be touched")

    def __next__(self) -> Union[str, bytes]:
        raise AssertionError("source must not be touched")


class UntouchableDestination:
    """
    Writable object that fails loudly if anything writes to it.
    """

    def __init__(self) -> None:
        self.touched = False

    def write(self, data: Any) -> int:
        self.touched = True
        raise AssertionError("destination must not be touched")


def as_writable(destination: UntouchableDestination) -> Any:
    """
    Present the sentinel as a file-like object to the type checker.
    """
    return destination


def test_reader_codec_config_snapshot() -> None:
    reader = jsonlines.Reader(io.StringIO("1\n"))
    config = reader.codec_config
    assert isinstance(config, jsonlines.CodecConfig)
    assert config.direction == "loads"
    assert not config.is_custom
    assert isinstance(config.backend, jsonlines.Backend)
    assert isinstance(config.capabilities, jsonlines.CodecCapabilities)
    module = jsonlines.jsonlines._backend_module(config.backend)
    assert module is not None
    assert config.func is module.loads
    # Readers have no write-side options.
    assert config.compact is None
    assert config.sort_keys is None


def test_writer_codec_config_snapshot() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, compact=True, sort_keys=True)
    config = writer.codec_config
    assert config.direction == "dumps"
    assert not config.is_custom
    assert config.backend is jsonlines.Backend.JSON
    assert config.capabilities.result_type == "str"
    assert config.compact is True
    assert config.sort_keys is True


def test_codec_config_is_frozen() -> None:
    config = jsonlines.Reader(io.StringIO("")).codec_config
    with pytest.raises(attr.exceptions.FrozenInstanceError):
        config.backend = jsonlines.Backend.JSON  # type: ignore[misc]


def test_codec_config_equality_ignores_callable_identity() -> None:
    first = jsonlines.Reader(io.StringIO("1\n")).codec_config
    second = jsonlines.Reader(io.StringIO("2\n")).codec_config
    assert first == second
    assert hash(first) == hash(second)


def test_writer_default_backend_is_stdlib() -> None:
    """
    Encoding must not silently switch to an accelerator library.
    """
    assert jsonlines.get_default_backend("dumps") is jsonlines.Backend.JSON
    writer = jsonlines.Writer(io.BytesIO())
    assert writer.codec_config.backend is jsonlines.Backend.JSON
    assert writer.codec_config.capabilities.result_type == "str"


def test_reader_default_backend_uses_fastest_available() -> None:
    reader = jsonlines.Reader(io.StringIO("1\n"))
    expected = jsonlines.get_default_backend("loads")
    assert reader.codec_config.backend is expected
    if jsonlines.jsonlines.orjson is not None:
        assert expected is jsonlines.Backend.ORJSON
    elif jsonlines.jsonlines.ujson is not None:
        assert expected is jsonlines.Backend.UJSON
    else:
        assert expected is jsonlines.Backend.JSON


@pytest.mark.parametrize("backend", INSTALLED_BACKENDS)
def test_explicit_backend_reader(backend: jsonlines.Backend) -> None:
    reader = jsonlines.Reader(io.StringIO('{"a": 1}\n'), backend=backend)
    assert reader.codec_config.backend is backend
    assert reader.read() == {"a": 1}


@pytest.mark.parametrize("backend", INSTALLED_BACKENDS)
def test_explicit_backend_writer(backend: jsonlines.Backend) -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, backend=backend)
    assert writer.codec_config.backend is backend
    writer.write({"a": 1})
    writer.close()
    assert fp.getvalue() in (b'{"a": 1}\n', b'{"a":1}\n')


def test_explicit_backend_by_name() -> None:
    reader = jsonlines.Reader(io.StringIO("1\n"), backend="json")
    assert reader.codec_config.backend is jsonlines.Backend.JSON
    writer = jsonlines.Writer(io.BytesIO(), backend="json")
    assert writer.codec_config.backend is jsonlines.Backend.JSON


def test_explicit_backend_through_open() -> None:
    with tempfile.TemporaryDirectory() as tmpdir:
        path = f"{tmpdir}/data.jsonl"
        with jsonlines.open(path, mode="w", backend="json") as writer:
            assert writer.codec_config.backend is jsonlines.Backend.JSON
            writer.write(123)
            writer.write(456)
        with open(path, "rb") as fp:
            assert fp.read().splitlines() == [b"123", b"456"]
        with jsonlines.open(path, mode="r", backend="json") as reader:
            assert reader.codec_config.backend is jsonlines.Backend.JSON
            assert list(reader) == [123, 456]


def test_missing_backend_is_reported() -> None:
    """
    Requesting a backend that is not installed fails up front, before any data
    is read or written.
    """
    for other in (jsonlines.Backend.ORJSON, jsonlines.Backend.UJSON):
        if other in INSTALLED_BACKENDS:
            continue
        source = UntouchableSource()
        with pytest.raises(jsonlines.CodecConfigError) as excinfo:
            jsonlines.Reader(source, backend=other)
        assert "not available" in str(excinfo.value)
        assert not source.touched

        destination = UntouchableDestination()
        with pytest.raises(jsonlines.CodecConfigError):
            jsonlines.Writer(as_writable(destination), backend=other)
        assert not destination.touched


def test_unknown_backend_is_reported() -> None:
    with pytest.raises(jsonlines.CodecConfigError) as excinfo:
        jsonlines.Reader(io.StringIO("1\n"), backend="not-a-backend")
    assert "unknown json backend" in str(excinfo.value)
    assert isinstance(excinfo.value, ValueError)
    assert isinstance(excinfo.value, jsonlines.Error)


@pytest.mark.parametrize("value", [1, object(), ["json"]])
def test_invalid_backend_type_is_reported(value: Any) -> None:
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.Reader(io.StringIO("1\n"), backend=value)


def test_custom_backend_value_cannot_be_selected() -> None:
    with pytest.raises(jsonlines.CodecConfigError) as excinfo:
        jsonlines.Reader(io.StringIO("1\n"), backend=jsonlines.Backend.CUSTOM)
    assert "cannot be selected explicitly" in str(excinfo.value)


def test_set_default_backend_can_be_reset() -> None:
    jsonlines.set_default_backend(None)
    jsonlines.set_default_backend("json")
    assert jsonlines.get_default_backend() is jsonlines.Backend.JSON
    assert jsonlines.get_default_backend("dumps") is jsonlines.Backend.JSON
    jsonlines.set_default_backend(None)


def test_set_default_backend_rejects_invalid_values() -> None:
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.set_default_backend("nope")
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.set_default_backend(42)  # type: ignore[arg-type]
    # The failed attempts leave the previous state alone.
    assert jsonlines.get_default_backend("dumps") is jsonlines.Backend.JSON


def test_get_default_backend_rejects_invalid_direction() -> None:
    with pytest.raises(ValueError):
        jsonlines.get_default_backend("both")  # type: ignore[arg-type]


def test_existing_reader_keeps_backend_after_default_change() -> None:
    reader = jsonlines.Reader(io.StringIO("1\n2\n"))
    original = reader.codec_config

    assert reader.read() == 1
    for other in INSTALLED_BACKENDS:
        jsonlines.set_default_backend(other)
        assert reader.codec_config == original
        assert reader.codec_config.backend is original.backend

    jsonlines.set_default_backend(None)
    assert reader.codec_config == original
    # Still reading with the original codec after all those changes.
    assert reader.read() == 2

    # A new reader does follow the current default.
    jsonlines.set_default_backend("json")
    assert (
        jsonlines.Reader(io.StringIO("1\n")).codec_config.backend
        is jsonlines.Backend.JSON
    )


def test_existing_writer_keeps_backend_after_default_change() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp)
    original = writer.codec_config

    jsonlines.set_default_backend("json")
    assert writer.codec_config == original
    writer.write(1)
    writer.close()
    assert fp.getvalue() == b"1\n"


def test_existing_object_keeps_custom_codec_after_default_change() -> None:
    loads_calls: List[Union[str, bytes]] = []

    def custom_loads(line: Union[str, bytes]) -> str:
        loads_calls.append(line)
        return "decoded"

    reader = jsonlines.Reader(io.StringIO('"whatever"\n'), loads=custom_loads)
    jsonlines.set_default_backend("json")
    assert reader.codec_config.backend is jsonlines.Backend.CUSTOM
    assert reader.read() == "decoded"
    assert loads_calls == ['"whatever"\n']


def test_custom_loads_is_not_overridden_by_probing() -> None:
    def custom_loads(line: Union[str, bytes]) -> str:
        return "spam"

    reader = jsonlines.Reader(io.BytesIO(b"{}\n"), loads=custom_loads)
    config = reader.codec_config
    assert config.is_custom
    assert config.backend is jsonlines.Backend.CUSTOM
    assert config.func is custom_loads
    assert config.capabilities.nan == "unknown"
    assert config.capabilities.result_type is None
    assert reader.read() == "spam"


def test_custom_dumps_is_not_called_with_synthetic_data() -> None:
    calls: List[Any] = []

    def custom_dumps(obj: Any) -> str:
        calls.append(obj)
        return "encoded"

    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, dumps=custom_dumps)
    assert calls == []
    writer.write({"a": 1})
    writer.write({"b": 2})
    writer.close()
    assert calls == [{"a": 1}, {"b": 2}]
    assert fp.getvalue() == b"encoded\nencoded\n"
    assert writer.codec_config.is_custom
    assert writer.codec_config.backend is jsonlines.Backend.CUSTOM


def test_custom_dumps_bytes_is_not_rewritten() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, dumps=lambda obj: b"raw-bytes")
    writer.write(1)
    writer.close()
    assert fp.getvalue() == b"raw-bytes\n"
    assert writer.codec_config.capabilities.result_type is None


def test_custom_dumps_bytes_on_text_stream_is_decoded() -> None:
    fp = io.StringIO()
    writer = jsonlines.Writer(fp, dumps=lambda obj: b"raw-bytes")
    writer.write(1)
    writer.close()
    assert fp.getvalue() == "raw-bytes\n"


def test_backend_cannot_be_combined_with_custom_loads() -> None:
    source = UntouchableSource()
    with pytest.raises(jsonlines.CodecConfigError) as excinfo:
        jsonlines.Reader(source, loads=lambda line: line, backend="json")
    assert "cannot be combined" in str(excinfo.value)
    assert not source.touched


def test_backend_cannot_be_combined_with_custom_dumps() -> None:
    destination = UntouchableDestination()
    with pytest.raises(jsonlines.CodecConfigError) as excinfo:
        jsonlines.Writer(as_writable(destination), dumps=lambda obj: "", backend="json")
    assert "cannot be combined" in str(excinfo.value)
    assert not destination.touched


def test_non_callable_loads_is_rejected() -> None:
    with pytest.raises(jsonlines.CodecConfigError) as excinfo:
        jsonlines.Reader(
            io.StringIO("1\n"), loads="not callable"  # type: ignore[arg-type]
        )
    assert "must be a callable" in str(excinfo.value)


def test_non_callable_dumps_is_rejected_before_writing() -> None:
    destination = UntouchableDestination()
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.Writer(as_writable(destination), dumps=42)  # type: ignore[arg-type]
    assert not destination.touched


def test_invalid_config_is_reported_before_source_is_touched() -> None:
    source = UntouchableSource()
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.Reader(source, backend="nope")
    assert not source.touched


def test_open_validates_config_before_touching_file() -> None:
    with tempfile.TemporaryDirectory() as tmpdir:
        target = f"{tmpdir}/should-not-exist.jsonl"
        with pytest.raises(jsonlines.CodecConfigError):
            jsonlines.open(target, mode="w", backend="nope")
        # The destination was neither created nor truncated.
        assert not os.path.exists(target)


def test_open_rejects_backend_with_custom_loads_before_opening() -> None:
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.open(
            "definitely-not-a-file.jsonl",
            mode="w",
            dumps=lambda obj: "",
            backend="json",
        )


@pytest.mark.skipif(
    jsonlines.jsonlines.orjson is None, reason="orjson is not installed"
)
def test_orjson_capabilities_match_observed_behaviour() -> None:
    reader = jsonlines.Reader(io.StringIO("1\n"), backend="orjson")
    caps = reader.codec_config.capabilities
    assert caps.nan == "rejected"
    assert caps.infinity == "rejected"
    assert caps.big_integers == "lossy"
    # The reported rejection is real, and the record is not rewritten.
    with pytest.raises(jsonlines.InvalidLineError):
        jsonlines.Reader(io.StringIO("NaN\n"), backend="orjson").read()

    writer = jsonlines.Writer(io.BytesIO(), backend="orjson")
    dumps_caps = writer.codec_config.capabilities
    assert dumps_caps.result_type == "bytes"
    assert dumps_caps.nan == "substituted"
    assert dumps_caps.supports_compact_output is False


@pytest.mark.skipif(
    jsonlines.jsonlines.orjson is None, reason="orjson is not installed"
)
def test_orjson_nan_is_written_as_null() -> None:
    """
    The substitution is the backend's own behaviour; the library reports it in
    the capability snapshot instead of rewriting the record itself.
    """
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, backend="orjson")
    writer.write({"x": float("nan")})
    writer.close()
    assert fp.getvalue() == b'{"x":null}\n'


@pytest.mark.skipif(jsonlines.jsonlines.ujson is None, reason="ujson is not installed")
def test_ujson_capabilities_match_observed_behaviour() -> None:
    reader = jsonlines.Reader(io.StringIO("NaN\n"), backend="ujson")
    assert reader.codec_config.capabilities.nan == "supported"
    value = reader.read()
    assert value != value  # NaN

    writer = jsonlines.Writer(io.BytesIO(), backend="ujson")
    caps = writer.codec_config.capabilities
    assert caps.result_type == "str"
    assert caps.nan == "supported"


def test_json_capabilities_match_observed_behaviour() -> None:
    reader = jsonlines.Reader(io.StringIO("NaN\n"), backend="json")
    assert reader.codec_config.capabilities.nan == "supported"
    value = reader.read()
    assert value != value  # NaN

    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, backend="json")
    writer.write({"n": float("nan")})
    writer.close()
    assert fp.getvalue() == b'{"n": NaN}\n'
    assert writer.codec_config.capabilities.nan == "supported"


def test_writer_large_integer_uses_reported_capability() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, backend="json")
    writer.write(10**30)
    writer.close()
    assert fp.getvalue() == b"1000000000000000000000000000000\n"


@pytest.mark.skipif(
    jsonlines.jsonlines.orjson is None, reason="orjson is not installed"
)
def test_orjson_large_integer_reports_rejection() -> None:
    writer = jsonlines.Writer(io.BytesIO(), backend="orjson")
    assert writer.codec_config.capabilities.big_integers == "rejected"
    # The error comes from the backend itself; records are not pre-processed.
    with pytest.raises(TypeError):
        writer.write(10**30)


@pytest.mark.skipif(
    jsonlines.jsonlines.orjson is None, reason="orjson is not installed"
)
def test_orjson_writer_options() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, backend="orjson", sort_keys=True)
    assert writer.codec_config.capabilities.supports_sort_keys is True
    writer.write({"b": 2, "a": 1})
    writer.close()
    assert fp.getvalue() == b'{"a":1,"b":2}\n'

    # orjson cannot produce non-compact output; that is reported, not hidden.
    writer = jsonlines.Writer(io.BytesIO(), backend="orjson", compact=False)
    assert writer.codec_config.capabilities.supports_compact_output is False


@pytest.mark.skipif(jsonlines.jsonlines.ujson is None, reason="ujson is not installed")
def test_ujson_writer_options() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, backend="ujson", compact=True, sort_keys=True)
    caps = writer.codec_config.capabilities
    assert caps.supports_compact_output is True
    assert caps.supports_sort_keys is True
    writer.write({"b": 2, "a": 1})
    writer.close()
    assert fp.getvalue() == b'{"a":1,"b":2}\n'


def test_codec_config_lifetime_is_independent_of_module_availability(
    monkeypatch: Any,
) -> None:
    """
    Even if an optional module disappears from the module namespace, an
    existing reader keeps using the codec it captured at creation time.
    """
    reader = jsonlines.Reader(io.StringIO("1\n2\n"))
    original = reader.codec_config
    monkeypatch.setattr(jsonlines.jsonlines, "orjson", None)
    monkeypatch.setattr(jsonlines.jsonlines, "ujson", None)
    assert reader.codec_config == original
    assert reader.read() == 1
    assert reader.read() == 2


def test_automatic_default_reports_writer_and_reader_choices() -> None:
    jsonlines.set_default_backend(None)
    assert jsonlines.get_default_backend("dumps") is jsonlines.Backend.JSON
    reader = jsonlines.Reader(io.StringIO("1\n"))
    writer = jsonlines.Writer(io.BytesIO())
    assert reader.codec_config.backend is jsonlines.get_default_backend("loads")
    assert writer.codec_config.backend is jsonlines.get_default_backend("dumps")


def test_pinned_default_that_became_unavailable_is_reported(
    monkeypatch: Any,
) -> None:
    """
    A pinned backend that is no longer usable fails explicitly for new objects
    instead of silently falling back to a different codec.
    """
    jsonlines.set_default_backend("json")
    monkeypatch.setattr(jsonlines.jsonlines, "json", None)
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.Reader(io.StringIO("1\n"))
    with pytest.raises(jsonlines.CodecConfigError):
        jsonlines.Writer(io.BytesIO())


def test_open_does_not_truncate_existing_file_on_invalid_config() -> None:
    with tempfile.TemporaryDirectory() as tmpdir:
        path = f"{tmpdir}/existing.jsonl"
        with open(path, "w", encoding="utf-8") as fp:
            fp.write("keep me\n")
        with pytest.raises(jsonlines.CodecConfigError):
            jsonlines.open(path, mode="w", backend="nope")
        with open(path, encoding="utf-8") as fp:
            assert fp.read() == "keep me\n"


def test_inapplicable_open_options_are_rejected_before_opening() -> None:
    # Options for the other direction are rejected, like the strict
    # Reader/Writer signatures do, and before any file is opened.
    with pytest.raises(TypeError):
        jsonlines.open("missing-file.jsonl", mode="w", loads=json.loads)
    with pytest.raises(TypeError):
        jsonlines.open("missing-file.jsonl", mode="r", compact=True)
