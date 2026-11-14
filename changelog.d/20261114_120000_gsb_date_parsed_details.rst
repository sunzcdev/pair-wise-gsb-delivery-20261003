Added
-----

*   Add ``*_parsed_detail`` mappings alongside each ``*_parsed`` date 9-tuple
    for RSS, Atom, and JSON Feed date elements. The details record the
    original date text, the recognized precision (``date``, ``minute``,
    ``second``, or ``subsecond``), whether a timezone was explicitly
    declared, a normalized UTC ``datetime`` (only when a timezone was
    explicitly declared, never from the machine's local timezone settings),
    and the failure reason when a value is unrecognized or contains invalid
    or out-of-range components. Invalid values like February 29 on a
    non-leap year are no longer silently corrected to another date in these
    details.

Fixed
-----

*   Parse the JSON Feed ``date_modified`` item field into ``updated`` and
    ``updated_parsed``; the parser previously checked for a nonexistent
    ``date_updated`` field, so modified dates were never parsed.
