``skipped_entries``
===================

A list of diagnostics for JSON Feed entries that were skipped because
their structure or field types did not conform to the JSON Feed format
while parsing with ``tolerant_entries=True``.

Each diagnostic is a dictionary with three keys:

``index``
    The zero-based position of the entry in the source ``items`` array.
``path``
    A JSON Pointer-like path to the offending value, for example
    ``items/2/attachments/0/url``.
``reason``
    A human-readable explanation of why the entry was skipped.

The list is present (and empty) for JSON feeds parsed with the default
settings, and it is absent for XML feeds.

See :ref:`advanced.json-tolerant-entries` for more details.
