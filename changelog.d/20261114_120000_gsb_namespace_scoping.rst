Fixed
-----

*   Treat namespace prefix bindings as element-scoped.  A prefix (or the
    default namespace) that is rebound on an element is now restored to
    the enclosing scope's binding when that element ends, so sibling
    entries are no longer routed to the wrong extension handler.
    Default namespace rebinding, ``xmlns=""`` undeclarations, and nested
    overrides are supported in both the strict and the loose parser, and
    ill-formed documents cannot leak one entry's local bindings into the
    next entry.  The ``namespaces`` result remains a document-level
    summary and keeps the first (outermost) binding when a prefix is
    rebound; conflicting bindings are recorded as parser diagnostics.
