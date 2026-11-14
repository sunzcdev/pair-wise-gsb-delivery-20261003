import hashlib
import json
from os import path

import pytest

from fastavro.repository import (
    FlatDictRepository,
    PackagedRepository,
    SchemaRepositoryError,
    export_schema_package,
)
from fastavro.schema import load_schema


def write_schema(directory, name, schema):
    with open(path.join(str(directory), f"{name}.avsc"), "w") as schema_file:
        json.dump(schema, schema_file)


def make_repo(tmp_path, schemas):
    repo_dir = tmp_path / "repo"
    repo_dir.mkdir()
    for name, schema in schemas.items():
        write_schema(repo_dir, name, schema)
    return FlatDictRepository(str(repo_dir))


PARENT = {
    "type": "record",
    "name": "Parent",
    "namespace": "namespace",
    "fields": [{"name": "child", "type": "Child"}],
}

CHILD = {
    "type": "record",
    "namespace": "namespace",
    "name": "Child",
    "fields": [{"name": "foo", "type": "string"}],
}


def test_export_package_contents(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Parent": PARENT, "namespace.Child": CHILD})
    package_dir = tmp_path / "package"

    manifest = export_schema_package("namespace.Parent", repo, str(package_dir))

    assert manifest["format_version"] == 1
    assert manifest["root"] == "namespace.Parent"
    assert set(manifest["schemas"]) == {"namespace.Parent", "namespace.Child"}

    for name, entry in manifest["schemas"].items():
        schema_file = package_dir / entry["file"]
        assert schema_file.exists()
        with open(schema_file) as fo:
            content = json.load(fo)
        expected = PARENT if name == "namespace.Parent" else CHILD
        assert content == expected
        canonical = json.dumps(expected, sort_keys=True, separators=(",", ":"))
        assert entry["sha256"] == hashlib.sha256(canonical.encode()).hexdigest()


def test_export_and_load_round_trip(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Parent": PARENT, "namespace.Child": CHILD})
    package_dir = tmp_path / "package"
    export_schema_package("namespace.Parent", repo, str(package_dir))

    packaged_repo = PackagedRepository(str(package_dir))
    assert packaged_repo.root == "namespace.Parent"

    parsed_from_package = load_schema(packaged_repo.root, repo=packaged_repo)
    parsed_from_source = load_schema("namespace.Parent", repo=repo)
    assert parsed_from_package == parsed_from_source


def test_export_allows_recursive_cycles(tmp_path):
    schemas = {
        "A": {
            "type": "record",
            "name": "A",
            "fields": [{"name": "b", "type": "B"}],
        },
        "B": {
            "type": "record",
            "name": "B",
            "fields": [{"name": "a", "type": "A"}],
        },
    }
    repo = make_repo(tmp_path, schemas)
    package_dir = tmp_path / "package"

    manifest = export_schema_package("A", repo, str(package_dir))
    assert set(manifest["schemas"]) == {"A", "B"}

    packaged_repo = PackagedRepository(str(package_dir))
    parsed = load_schema("A", repo=packaged_repo)
    assert parsed["name"] == "A"


def test_export_allows_self_reference(tmp_path):
    schemas = {
        "LinkedNode": {
            "type": "record",
            "name": "LinkedNode",
            "fields": [
                {"name": "value", "type": "int"},
                {"name": "next", "type": ["null", "LinkedNode"], "default": None},
            ],
        }
    }
    repo = make_repo(tmp_path, schemas)
    package_dir = tmp_path / "package"

    manifest = export_schema_package("LinkedNode", repo, str(package_dir))
    assert set(manifest["schemas"]) == {"LinkedNode"}

    packaged_repo = PackagedRepository(str(package_dir))
    parsed = load_schema("LinkedNode", repo=packaged_repo)
    assert parsed["name"] == "LinkedNode"


def test_export_rejects_conflicting_definitions(tmp_path):
    def parent_with_inline_child(field_type):
        return {
            "type": "record",
            "name": "Parent",
            "fields": [
                {
                    "name": "child",
                    "type": {
                        "type": "record",
                        "name": "Child",
                        "fields": [{"name": "foo", "type": field_type}],
                    },
                }
            ],
        }

    schemas = {
        "Root": {
            "type": "record",
            "name": "Root",
            "fields": [
                {"name": "first", "type": "First"},
                {"name": "second", "type": "Second"},
            ],
        },
        "First": parent_with_inline_child("string"),
        "Second": parent_with_inline_child("int"),
    }
    # Both First and Second define an inline record named "Parent" but with
    # different definitions; the export must reject this
    repo = make_repo(tmp_path, schemas)

    with pytest.raises(SchemaRepositoryError) as err:
        export_schema_package("Root", repo, str(tmp_path / "package"))

    assert "Conflicting definitions" in str(err.value)


def test_export_only_includes_reachable_dependencies(tmp_path):
    repo = make_repo(
        tmp_path,
        {
            "namespace.Parent": PARENT,
            "namespace.Child": CHILD,
            "namespace.Unrelated": {
                "type": "record",
                "namespace": "namespace",
                "name": "Unrelated",
                "fields": [],
            },
        },
    )
    package_dir = tmp_path / "package"
    manifest = export_schema_package("namespace.Parent", repo, str(package_dir))

    assert "namespace.Unrelated" not in manifest["schemas"]
    assert not (package_dir / "schemas" / "namespace.Unrelated.avsc").exists()


def test_export_preserves_namespaces(tmp_path):
    # A relative reference to "Child" from namespace "namespace" must resolve
    # to "namespace.Child" and must not be hoisted to a global "Child"
    repo = make_repo(
        tmp_path,
        {
            "namespace.Parent": PARENT,
            "namespace.Child": CHILD,
            "Child": {
                "type": "record",
                "name": "Child",
                "fields": [{"name": "bar", "type": "int"}],
            },
        },
    )
    package_dir = tmp_path / "package"
    manifest = export_schema_package("namespace.Parent", repo, str(package_dir))

    assert set(manifest["schemas"]) == {"namespace.Parent", "namespace.Child"}

    packaged_repo = PackagedRepository(str(package_dir))
    assert packaged_repo.load("namespace.Child") == CHILD
    with pytest.raises(SchemaRepositoryError):
        packaged_repo.load("Child")


def test_export_fails_when_dependency_is_missing(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Parent": PARENT})

    with pytest.raises(SchemaRepositoryError) as err:
        export_schema_package("namespace.Parent", repo, str(tmp_path / "package"))

    assert "namespace.Child" in str(err.value)


def test_load_fails_when_schema_file_is_missing(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Parent": PARENT, "namespace.Child": CHILD})
    package_dir = tmp_path / "package"
    export_schema_package("namespace.Parent", repo, str(package_dir))

    (package_dir / "schemas" / "namespace.Child.avsc").unlink()

    with pytest.raises(SchemaRepositoryError) as err:
        PackagedRepository(str(package_dir))

    assert "namespace.Child" in str(err.value)


def test_load_fails_when_schema_content_was_replaced(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Parent": PARENT, "namespace.Child": CHILD})
    package_dir = tmp_path / "package"
    export_schema_package("namespace.Parent", repo, str(package_dir))

    write_schema(
        package_dir / "schemas",
        "namespace.Child",
        {"type": "record", "name": "Evil", "fields": []},
    )

    with pytest.raises(SchemaRepositoryError) as err:
        PackagedRepository(str(package_dir))

    assert "Digest mismatch" in str(err.value)


def test_load_fails_when_manifest_is_missing(tmp_path):
    package_dir = tmp_path / "package"
    package_dir.mkdir()

    with pytest.raises(SchemaRepositoryError) as err:
        PackagedRepository(str(package_dir))

    assert "manifest" in str(err.value)


def test_load_fails_on_unsupported_format_version(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Parent": PARENT, "namespace.Child": CHILD})
    package_dir = tmp_path / "package"
    export_schema_package("namespace.Parent", repo, str(package_dir))

    manifest_path = package_dir / "manifest.json"
    with open(manifest_path) as fo:
        manifest = json.load(fo)
    manifest["format_version"] = 999
    with open(manifest_path, "w") as fo:
        json.dump(manifest, fo)

    with pytest.raises(SchemaRepositoryError) as err:
        PackagedRepository(str(package_dir))

    assert "format version" in str(err.value)


def test_load_fails_for_schema_not_in_package(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Parent": PARENT, "namespace.Child": CHILD})
    package_dir = tmp_path / "package"
    export_schema_package("namespace.Parent", repo, str(package_dir))

    packaged_repo = PackagedRepository(str(package_dir))
    with pytest.raises(SchemaRepositoryError) as err:
        packaged_repo.load("namespace.Missing")

    assert "namespace.Missing" in str(err.value)


def test_loaded_schemas_are_copies(tmp_path):
    repo = make_repo(tmp_path, {"namespace.Child": CHILD})
    package_dir = tmp_path / "package"
    export_schema_package("namespace.Child", repo, str(package_dir))

    packaged_repo = PackagedRepository(str(package_dir))
    loaded = packaged_repo.load("namespace.Child")
    loaded["fields"].append({"name": "injected", "type": "int"})

    assert packaged_repo.load("namespace.Child") == CHILD
