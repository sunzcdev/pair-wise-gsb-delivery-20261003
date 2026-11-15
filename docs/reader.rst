fastavro.read
=============

.. autoclass:: fastavro._read_py.reader

.. autoclass:: fastavro._read_py.block_reader

.. autoclass:: fastavro._read_py.Block

.. autofunction:: fastavro._read_py.schemaless_reader

.. autofunction:: fastavro._read_py.is_avro

Parse explanations
------------------

When onboarding a new Avro source it can be useful to know *why* each output
record got the structure it has. The pure-Python ``reader``,
``block_reader``, ``schemaless_reader`` and ``json_reader`` accept an optional
``explain_callback``. The callback is invoked with an
:class:`~fastavro.read.ExplanationEvent` for every parse decision that is
**actually executed** while decoding a record:

* a writer field matched a reader field through an alias
  (``fastavro.read.FIELD_ALIAS``);
* a reader field missing from the writer was filled from its default value
  (``fastavro.read.FIELD_DEFAULT``);
* a union branch was selected (``fastavro.read.UNION_BRANCH``);
* a value was promoted from the writer type to the reader type
  (``fastavro.read.PROMOTION``).

Union branches that were not selected and writer fields skipped because the
reader schema has no matching field never produce events. Events carry the
zero-based ``record_index`` (continuous across blocks) and the schema
``path`` (record and field names; reader-schema names when a reader schema is
supplied). Field values are deliberately never included in an event, and when
the callback is not supplied no events are collected at all.

The binary container and schemaless entry points use identical path
semantics; a schemaless read decodes a single record so every event has
``record_index == 0``.

What happens when the callback raises is chosen explicitly with
``on_explain_error``:

* ``"raise"`` (the default): the exception propagates and reading stops;
* ``"ignore"``: the exception is swallowed and reading continues, returning
  exactly the same data as without a callback.

Example::

    from io import BytesIO
    import fastavro

    def explain(event):
        print(event.record_index, event.path, event.event_type, event.details)

    with open("some-file.avro", "rb") as fo:
        for record in fastavro.reader(
            fo, reader_schema, explain_callback=explain
        ):
            process_record(record)

.. autoclass:: fastavro._explain_py.ExplanationEvent
    :members:

