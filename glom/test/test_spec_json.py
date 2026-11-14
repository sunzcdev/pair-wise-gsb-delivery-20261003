import io
import json
import os
import tempfile

import pytest

import glom
from glom import (glom as glom_func, T, Path, Val, Coalesce, Check, Spec,
                  )
from glom import spec_json
from glom.spec_json import (SPEC_JSON_VERSION, encode_spec, dumps_spec,
                            dump_spec, load_spec, loads_spec, load_spec_file,
                            register_spec_type, render_spec_path,
                            SpecEncodeError, SpecDecodeError, SpecJSONError,
                            TYPE_REGISTRY)


TARGET = {
    "name": "widget",
    "count": "3",
    "meta": {"d.e": "dot", 2: "int-key", "tags": [10, 20, 30]},
    "items": [{"id": 1, "kind": "a"}, {"id": 2, "kind": "b"}],
}


def _roundtrip(spec):
    encoded = encode_spec(spec)
    text = dumps_spec(spec)
    # strict JSON: no NaN/Infinity and re-parseable
    json.loads(text)
    loaded = loads_spec(text)
    assert encode_spec(loaded) == encoded  # idempotent
    return loaded, encoded


def test_envelope_version_and_strict_json():
    doc = encode_spec("a.b")
    assert doc["glom_spec_version"] == SPEC_JSON_VERSION == 1
    assert "spec" in doc
    # strict JSON rejects non-finite values
    with pytest.raises(ValueError):
        json.dumps(float("nan"), allow_nan=False)


def test_literal_and_path_are_distinct():
    # a bare string is a path; a Val-wrapped string is a literal
    path_loaded, _ = _roundtrip("name")
    literal_loaded, _ = _roundtrip(Val("name"))
    assert glom_func(TARGET, path_loaded) == "widget"
    assert glom_func(TARGET, literal_loaded) == "name"

    # integer and string keys stay faithful
    assert glom_func({1: "a", "1": "b"}, loads_spec(dumps_spec(Path(1)))) == "a"
    assert glom_func({1: "a", "1": "b"}, loads_spec(dumps_spec(Path("1")))) == "b"

    # bare constant normalizes to a Val node
    loaded = loads_spec(dumps_spec(Val(42)))
    assert type(loaded) is Val and loaded.value == 42 and type(loaded.value) is int
    assert loads_spec(dumps_spec(Val("42"))).value == "42"


def test_literal_containers_and_scalars():
    for value in [None, True, False, 1, 1.5, "text",
                  [1, "two", True, None], {"k": [1, 2], "j": {"n": 3}}]:
        loaded, _ = _roundtrip(Val(value))
        assert type(loaded) is Val
        assert loaded.value == value


def test_path_forms_and_t_equivalence():
    spec = {
        "dotted": "meta.tags.1",
        "explicit": Path("meta", "d.e"),
        "int_key": Path("meta", 2),
        "t_expr": T["meta"]["tags"][0],
        "mixed": T["meta"]["d.e"],
        "root": T,
        "empty": Path(),
    }
    loaded, _ = _roundtrip(spec)
    path_target = dict(TARGET)
    path_target["meta"] = dict(TARGET["meta"])
    assert glom_func(path_target, loaded) == glom_func(path_target, spec)
    # attribute vs item access is preserved on reload
    assert loads_spec(dumps_spec(T.a["b"].c)) == T.a["b"].c


def test_object_and_list_projection_roundtrip():
    # object projection (with typed keys) over a dict target
    obj_spec = {"typed_key": {2: Val("two-int-key"), "2": Val("two-str-key")},
                "name": "name"}
    obj_target = {2: "ignored", "2": "ignored", "name": "widget"}
    obj_loaded, _ = _roundtrip(obj_spec)
    assert glom_func(obj_target, obj_loaded) == glom_func(obj_target, obj_spec)

    # list projection over a list target
    list_spec = [{"id": "id", "is_int": Check(type=int, spec="id")}]
    list_loaded, _ = _roundtrip(list_spec)
    assert glom_func(TARGET["items"], list_loaded) == glom_func(
        TARGET["items"], list_spec)


def test_coalesce_defaults_and_skip():
    spec = {
        "with_default": Coalesce("absent", "missing", default="fallback"),
        "with_skip": Coalesce("a", "b", skip=None, default="d"),
        "plain": Coalesce("name", "count"),
        "nested_default": Coalesce(Val(7), default=0),
    }
    target = {"a": None, "name": "widget", "count": "3"}
    loaded, _ = _roundtrip(spec)
    assert glom_func(target, loaded) == glom_func(target, spec)


def test_check_variants():
    spec = {
        "exact": Check(type=str, spec="name"),
        "multi_type": Check(type=(str, int), spec="count"),
        "instance": Check(instance_of=(str, int), spec="count"),
        "equal": Check(equal_to="widget", spec="name"),
        "one_of": Check(one_of=("widget", "gadget"), spec="name"),
        "default": Check(type=int, default=-1, spec="count"),
        "target_only": Check(instance_of=dict),
    }
    loaded, _ = _roundtrip(spec)
    assert glom_func(TARGET, loaded) == glom_func(TARGET, spec)


def test_spec_wrapper_is_unwrapped():
    loaded, _ = _roundtrip(Spec(Val(1)))
    assert type(loaded) is Val and loaded.value == 1


def test_file_and_dict_interfaces():
    spec = {"a": Coalesce("x", default=0), "b": ["items"]}
    buf = io.StringIO()
    dump_spec(spec, buf)
    buf.seek(0)
    assert load_spec_file(buf) is not None

    fd, path = tempfile.mkstemp(suffix=".json", dir=os.getcwd())
    os.close(fd)
    try:
        with open(path, "w", encoding="utf8") as fp:
            dump_spec(spec, fp)
        with open(path, "r", encoding="utf8") as fp:
            loaded = load_spec_file(fp)
        assert encode_spec(loaded) == encode_spec(spec)
    finally:
        os.remove(path)

    # load_spec accepts either parsed data or a JSON string
    assert load_spec(encode_spec(spec)) is not None
    assert load_spec(dumps_spec(spec)) is not None


# --------------------------------------------------------------------------
# encoding errors: unsupported specs must report an in-spec path
# --------------------------------------------------------------------------

def test_encoding_rejects_unsupported_with_path():
    cases = [
        ({"a": lambda x: x}, "a"),
        ({"outer": [Coalesce("x", default_factory=list)]}, "outer"),
        (Check(validate=lambda x: True), "validate"),
        (Path(T["a"].upper()), "unsupported path operation"),
        (Check(type=complex), "type registry"),
        (Val(float("nan")), "non-finite"),
        ((1, 2), "unsupported spec type"),
        (glom.Pipe("a", "b"), "unsupported spec type"),
        (Spec(Val(1), scope={"x": 1}), "scope"),
        (Coalesce("a", skip=(1, 2)), "skip"),
        (Coalesce("a", skip_exc=KeyError), "skip_exc"),
        (Val({1: "x"}), "literal objects may only have string keys"),
        ({True: "x"}, "object projection keys"),
        ([1, 2], "exactly one subspec"),
    ]
    for spec, fragment in cases:
        with pytest.raises(SpecEncodeError) as exc_info:
            encode_spec(spec)
        assert fragment in str(exc_info.value)
        assert isinstance(exc_info.value.spec_path, tuple)


def test_encoding_error_paths_point_inside_spec():
    spec = {"good": "name", "bad": {"inner": lambda x: x}}
    with pytest.raises(SpecEncodeError) as exc_info:
        encode_spec(spec)
    assert exc_info.value.spec_path[0] == "bad"
    assert "inner" in str(exc_info.value)

    with pytest.raises(SpecEncodeError) as exc_info:
        encode_spec([Check(validate=int)])
    assert exc_info.value.spec_path[:2] == (0, "validate")


def test_encoding_rejects_star_and_scope_paths():
    with pytest.raises(SpecEncodeError):
        encode_spec("a.*")
    with pytest.raises(SpecEncodeError):
        encode_spec(glom.S.x)


# --------------------------------------------------------------------------
# loading errors: unknown version/op and duplicate fields
# --------------------------------------------------------------------------

def _doc(node, **extra):
    doc = {"glom_spec_version": SPEC_JSON_VERSION, "spec": node}
    doc.update(extra)
    return json.dumps(doc)


def test_loading_rejects_unknown_version():
    with pytest.raises(SpecDecodeError) as exc_info:
        loads_spec(json.dumps({"glom_spec_version": 2,
                               "spec": {"op": "val", "value": 1}}))
    assert "unknown spec JSON version" in str(exc_info.value)
    assert exc_info.value.spec_path == ("glom_spec_version",)

    with pytest.raises(SpecDecodeError):
        loads_spec(json.dumps({"spec": {"op": "val", "value": 1}}))


def test_loading_rejects_unknown_operation_and_fields():
    with pytest.raises(SpecDecodeError) as exc_info:
        loads_spec(_doc({"op": "frobnicate"}))
    assert "unknown operation" in str(exc_info.value)
    assert exc_info.value.spec_path == ("spec", "op")

    with pytest.raises(SpecDecodeError) as exc_info:
        loads_spec(_doc({"op": "val", "value": 1, "bogus": 2}))
    assert "unknown field" in str(exc_info.value)

    with pytest.raises(SpecDecodeError):
        loads_spec(_doc({"op": "val"}))  # missing value

    # duplicate "op" field within a node
    raw_op = '{"glom_spec_version": 1, "spec": {"op": "val", "op": "path"}}'
    with pytest.raises(SpecDecodeError) as exc_info:
        loads_spec(raw_op)
    assert "duplicate field" in str(exc_info.value)

    # non-string op
    with pytest.raises(SpecDecodeError):
        loads_spec(_doc({"op": 5}))


def test_loading_rejects_duplicate_fields_with_path():
    # duplicate top-level version key, detectable only at raw JSON level
    raw = ('{"glom_spec_version": 1, "glom_spec_version": 1,'
           ' "spec": {"op": "val", "value": 1}}')
    with pytest.raises(SpecDecodeError) as exc_info:
        loads_spec(raw)
    assert "duplicate field" in str(exc_info.value)

    # duplicate projection field (different encodings, same logical key)
    doc = {"op": "object", "fields": [
        [{"type": "str", "value": "k"}, {"op": "val", "value": 1}],
        [{"type": "str", "value": "k"}, {"op": "val", "value": 2}],
    ]}
    with pytest.raises(SpecDecodeError) as exc_info:
        loads_spec(_doc(doc))
    assert "duplicate field" in str(exc_info.value)
    assert "fields" in str(exc_info.value)


def test_loading_rejects_unknown_type():
    doc = {"op": "check", "types": ["complex"],
           "instance_of": [], "values": []}
    with pytest.raises(SpecDecodeError) as exc_info:
        loads_spec(_doc(doc))
    assert "unknown type name" in str(exc_info.value)


def test_loading_rejects_key_type_mismatch_and_bad_values():
    doc = {"op": "object", "fields": [
        [{"type": "int", "value": "1"}, {"op": "val", "value": 1}],
    ]}
    with pytest.raises(SpecDecodeError):
        loads_spec(_doc(doc))

    with pytest.raises(SpecDecodeError):
        loads_spec(_doc({"op": "path",
                         "steps": [{"op": "item", "value": True}]}))


def test_loading_invalid_json_and_shape():
    with pytest.raises(SpecDecodeError):
        loads_spec("{not valid json")
    with pytest.raises(SpecDecodeError):
        loads_spec(json.dumps(["not", "an", "envelope"]))
    with pytest.raises(SpecDecodeError):
        loads_spec(json.dumps({"glom_spec_version": 1, "spec": 5}))
    with pytest.raises(SpecDecodeError):
        loads_spec(b'{"glom_spec_version": 1}')  # bytes, not str
    with pytest.raises(SpecDecodeError):
        load_spec(123)
    # parsed dict still goes through strict validation
    assert type(load_spec(encode_spec(Val(1)))) is Val


def test_error_path_consistency_between_encode_and_load():
    # the same structural problem surfaces with a matching rendered location
    spec = {"outer": [Check(validate=int)]}
    with pytest.raises(SpecEncodeError) as enc:
        encode_spec(spec)
    rendered = render_spec_path(enc.value.spec_path)
    assert "outer" in rendered and "validate" in rendered


def test_exception_hierarchy_and_members():
    assert issubclass(SpecEncodeError, SpecJSONError)
    assert issubclass(SpecDecodeError, SpecJSONError)
    assert issubclass(SpecJSONError, glom.GlomError)
    assert issubclass(SpecJSONError, ValueError)
    err = SpecDecodeError("boom", ("a", 0, "b"))
    assert err.spec_path == ("a", 0, "b")
    assert "a" in str(err) and "boom" in str(err)
    assert render_spec_path(()) == "<root>"


def test_register_spec_type_roundtrip():
    class Marker(object):
        pass

    register_spec_type("marker", Marker)
    try:
        assert TYPE_REGISTRY["marker"] is Marker
        loaded = loads_spec(dumps_spec(Check(type=Marker)))
        assert loaded.types == (Marker,)
    finally:
        del TYPE_REGISTRY["marker"]

    with pytest.raises(ValueError):
        register_spec_type("", Marker)
    with pytest.raises(ValueError):
        register_spec_type("not-a-type", 42)


def test_no_executable_recovery_primitives():
    # loading only ever builds the documented, inert spec node types;
    # the module source must not rely on eval/exec/__import__/pickle.
    import inspect
    import ast

    source = inspect.getsource(spec_json)
    tree = ast.parse(source)

    imported = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            imported.update(alias.name for alias in node.names)
        elif isinstance(node, ast.ImportFrom):
            imported.add(node.module or "")
    assert "pickle" not in imported
    assert not any(name.startswith("importlib") for name in imported)

    called = {node.func.id for node in ast.walk(tree)
              if isinstance(node, ast.Call) and isinstance(node.func, ast.Name)}
    assert "eval" not in called
    assert "exec" not in called
    assert "__import__" not in called
