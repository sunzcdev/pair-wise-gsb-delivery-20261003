import hashlib
import json
import os
from copy import deepcopy
from os import path

from .._schema_common import PRIMITIVES, SchemaParseException
from .base import AbstractSchemaRepository, SchemaRepositoryError

PACKAGE_FORMAT_VERSION = 1
MANIFEST_FILE_NAME = "manifest.json"
SCHEMAS_DIR_NAME = "schemas"
FILE_EXT = "avsc"


def _canonical_json(schema):
    """Deterministic JSON serialization used for digests."""
    return json.dumps(schema, sort_keys=True, separators=(",", ":"))


def _content_digest(schema):
    return hashlib.sha256(_canonical_json(schema).encode("utf-8")).hexdigest()


def _schema_name(schema, parent_namespace):
    """Local copy of the avro fullname resolution rules.

    Duplicated here (instead of importing from fastavro._schema_py) to avoid a
    circular import, and to make sure nested names keep their namespace rather
    than being hoisted to the global namespace.
    """
    name = schema["name"]
    namespace = schema.get("namespace", parent_namespace)
    if "." in name:
        return name.rsplit(".", 1)[0], name
    elif namespace:
        return namespace, f"{namespace}.{name}"
    else:
        return "", name


def _resolve_reference(name, namespace):
    if "." not in name and namespace:
        return f"{namespace}.{name}"
    return name


def _walk_schema(schema, namespace, on_define, on_reference):
    """Walk a raw (unparsed) schema.

    `on_define(fullname, subschema)` is called for every named type definition
    (record/error/enum/fixed) and `on_reference(fullname)` for every reference
    to a named type. Names are resolved against the enclosing namespace exactly
    like the parser does, so nested names are never hoisted to the global
    namespace.
    """
    if isinstance(schema, list):
        for sub_schema in schema:
            _walk_schema(sub_schema, namespace, on_define, on_reference)
    elif isinstance(schema, str):
        if schema not in PRIMITIVES:
            on_reference(_resolve_reference(schema, namespace))
    elif isinstance(schema, dict):
        schema_type = schema.get("type")
        if schema_type is None:
            raise SchemaParseException(f'"type" is missing from the schema: {schema}')

        if isinstance(schema_type, (dict, list)):
            # e.g. {"type": {"type": "record", ...}} or {"type": [...]}
            _walk_schema(schema_type, namespace, on_define, on_reference)
        elif schema_type == "array":
            _walk_schema(schema["items"], namespace, on_define, on_reference)
        elif schema_type == "map":
            _walk_schema(schema["values"], namespace, on_define, on_reference)
        elif schema_type == "record" or schema_type == "error":
            record_namespace, fullname = _schema_name(schema, namespace)
            on_define(fullname, schema)
            for field in schema.get("fields", []):
                _walk_schema(
                    field["type"], record_namespace, on_define, on_reference
                )
        elif schema_type == "enum" or schema_type == "fixed":
            _, fullname = _schema_name(schema, namespace)
            on_define(fullname, schema)
        elif schema_type in PRIMITIVES:
            pass
        else:
            # {"type": "SomeName"}; a reference to a named type
            on_reference(_resolve_reference(schema_type, namespace))
    else:
        raise SchemaParseException(f"Invalid schema: {schema}")


def _validate_package_key(key):
    if (
        not key
        or key in (".", "..")
        or "/" in key
        or "\\" in key
        or "\x00" in key
    ):
        raise SchemaRepositoryError(f"Invalid schema name for a package: {key!r}")


def export_schema_package(root_name, repo, output_dir):
    """Exports a root schema and all of its reachable dependencies as an
    offline schema package.

    The package is a directory containing a `manifest.json` (with the package
    format version, the root schema name and a sha256 digest for every schema
    file) plus one `.avsc` file per exported schema. Only dependencies
    reachable from the root schema are exported. Recursive references
    (including cycles) are allowed, but if the same fullname is encountered
    with two different definitions an error is raised.

    Parameters
    ----------
    root_name
        Name of the root schema, loadable via `repo`
    repo
        An AbstractSchemaRepository used to resolve dependencies
    output_dir
        Directory the package will be written to


    Example::

        from fastavro.repository import FlatDictRepository, export_schema_package

        repo = FlatDictRepository("path/to/schemas")
        export_schema_package("namespace.Parent", repo, "path/to/package")
    """
    definitions = {}  # fullname -> defining (sub)schema
    files = {}  # package key -> schema to write as its own file
    order = []

    def on_define(fullname, sub_schema):
        existing = definitions.get(fullname)
        if existing is not None:
            if _canonical_json(existing) != _canonical_json(sub_schema):
                raise SchemaRepositoryError(
                    f"Conflicting definitions for the named schema '{fullname}'"
                )
        else:
            definitions[fullname] = sub_schema

    def add_file(key, schema):
        _validate_package_key(key)
        if key not in files:
            files[key] = schema
            order.append(key)

    root_schema = repo.load(root_name)
    if isinstance(root_schema, dict) and "name" in root_schema:
        root_key = _schema_name(root_schema, "")[1]
    else:
        root_key = root_name
    add_file(root_key, root_schema)

    pending = []
    _walk_schema(root_schema, "", on_define, pending.append)

    errors = {}
    while pending:
        resolved_any = False
        still_pending = []
        for reference in pending:
            if reference in files:
                # Already exported as a standalone file
                resolved_any = True
                continue
            if reference in definitions:
                # Defined inline inside another exported schema. Export it as
                # its own file as well so that the package is self contained
                # no matter which order schemas get parsed in.
                add_file(reference, definitions[reference])
                resolved_any = True
                continue
            try:
                dependency = repo.load(reference)
            except SchemaRepositoryError as error:
                # The definition might show up inline in a dependency we have
                # not walked yet, so defer it instead of failing right away
                errors[reference] = error
                still_pending.append(reference)
                continue
            if isinstance(dependency, dict) and "name" in dependency:
                dependency_key = _schema_name(dependency, "")[1]
            else:
                dependency_key = reference
            add_file(dependency_key, dependency)
            new_references = []
            _walk_schema(dependency, "", on_define, new_references.append)
            still_pending.extend(new_references)
            resolved_any = True
        if not still_pending:
            break
        if not resolved_any:
            missing = still_pending[0]
            raise SchemaRepositoryError(
                f"Failed to load '{missing}' schema while exporting "
                f"'{root_name}'"
            ) from errors[missing]
        pending = still_pending

    schemas_dir = path.join(output_dir, SCHEMAS_DIR_NAME)
    os.makedirs(schemas_dir, exist_ok=True)

    manifest = {
        "format_version": PACKAGE_FORMAT_VERSION,
        "root": root_key,
        "schemas": {},
    }
    for key in order:
        schema = files[key]
        file_name = f"{key}.{FILE_EXT}"
        relative_file = f"{SCHEMAS_DIR_NAME}/{file_name}"
        with open(path.join(schemas_dir, file_name), "w") as schema_file:
            json.dump(schema, schema_file, indent=2, sort_keys=True)
            schema_file.write("\n")
        manifest["schemas"][key] = {
            "file": relative_file,
            "sha256": _content_digest(schema),
        }

    with open(path.join(output_dir, MANIFEST_FILE_NAME), "w") as manifest_file:
        json.dump(manifest, manifest_file, indent=2, sort_keys=True)
        manifest_file.write("\n")

    return manifest


class PackagedRepository(AbstractSchemaRepository):
    """A repository that loads schemas from an offline schema package created
    by `export_schema_package`.

    Every digest in the manifest is verified when the repository is created;
    if a file is missing or its content was replaced, a SchemaRepositoryError
    is raised before any schema is handed out, so a partially usable package
    can never produce a half-parsed schema. Schemas are only decoded as JSON;
    nothing in the package is ever executed.

    Parameters
    ----------
    package_path
        Path to the package directory


    Example::

        from fastavro.repository import PackagedRepository
        from fastavro.schema import load_schema

        repo = PackagedRepository("path/to/package")
        parsed_schema = load_schema(repo.root, repo=repo)
    """

    def __init__(self, package_path):
        self.path = package_path
        manifest_path = path.join(package_path, MANIFEST_FILE_NAME)
        try:
            with open(manifest_path) as manifest_file:
                manifest = json.load(manifest_file)
        except IOError as error:
            raise SchemaRepositoryError(
                "Failed to load the schema package manifest",
            ) from error
        except json.decoder.JSONDecodeError as error:
            raise SchemaRepositoryError(
                "Failed to parse the schema package manifest",
            ) from error

        if not isinstance(manifest, dict):
            raise SchemaRepositoryError(
                "The schema package manifest is malformed",
            )

        format_version = manifest.get("format_version")
        if format_version != PACKAGE_FORMAT_VERSION:
            raise SchemaRepositoryError(
                f"Unsupported schema package format version: {format_version}"
            )

        try:
            self._root = manifest["root"]
            entries = manifest["schemas"]
        except (KeyError, TypeError) as error:
            raise SchemaRepositoryError(
                "The schema package manifest is malformed",
            ) from error
        if not isinstance(entries, dict):
            raise SchemaRepositoryError(
                "The schema package manifest is malformed",
            )

        package_dir = path.abspath(package_path)
        schemas = {}
        for name, entry in entries.items():
            try:
                relative_file = entry["file"]
                expected_digest = entry["sha256"]
            except (KeyError, TypeError) as error:
                raise SchemaRepositoryError(
                    f"The manifest entry for '{name}' is malformed",
                ) from error
            file_path = path.abspath(path.join(package_dir, relative_file))
            if path.commonpath([package_dir, file_path]) != package_dir:
                raise SchemaRepositoryError(
                    f"Schema file for '{name}' points outside of the package"
                )
            try:
                with open(file_path) as schema_file:
                    schema = json.load(schema_file)
            except IOError as error:
                raise SchemaRepositoryError(
                    f"Failed to load '{name}' schema from the package",
                ) from error
            except json.decoder.JSONDecodeError as error:
                raise SchemaRepositoryError(
                    f"Failed to parse '{name}' schema from the package",
                ) from error

            if _content_digest(schema) != expected_digest:
                raise SchemaRepositoryError(
                    f"Digest mismatch for '{name}' schema; "
                    "the package content was modified"
                )
            schemas[name] = schema

        self._schemas = schemas

    @property
    def root(self):
        """The fullname of the root schema of the package"""
        return self._root

    def load(self, name):
        try:
            # Return a copy so that callers (e.g. load_schema) can mutate the
            # schema without corrupting the repository
            return deepcopy(self._schemas[name])
        except KeyError:
            raise SchemaRepositoryError(
                f"Failed to load '{name}' schema; it is not part of the package",
            ) from None
