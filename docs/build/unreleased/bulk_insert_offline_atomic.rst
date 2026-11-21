.. change::
    :tags: bug, operations

    Fixed issue in offline (``--sql``) mode where
    :meth:`.Operations.bulk_insert` emitted each row's ``INSERT`` statement
    one at a time, such that a value that couldn't be rendered as an inline
    literal (for example an unsupported object on a custom type) raised a
    :class:`~sqlalchemy.exc.CompileError` only after the preceding rows had
    already been written to the SQL output, leaving a half-emitted batch with
    an error message that didn't identify the offending cell.  The full batch
    is now compiled before any statement is emitted, so either every
    ``INSERT`` is written or none is, and a failure reports the 1-based row
    number and column name, e.g.
    ``Could not render value for row 2, column 'custom' ...``.  The
    ``inline_literal`` value strategy for custom types is unchanged.
