.. change::
    :tags: bug, commands

    Fixed issue where a ``process_revision_directives`` callback that
    produces more than one revision in a single ``alembic revision`` run
    could leave partially written migration files on disk and visible to
    the :class:`.RevisionMap` if a post write hook failed on a later
    revision in the group.  The revisions generated within one run are now
    finalized as a single group: if writing any file or running its post
    write hooks fails, all of the revision files created by the run are
    removed and the revision map is reloaded from disk, allowing the whole
    group to be regenerated using the same revision ids.  Pre-existing
    files that happen to share a name with a generated file are never
    deleted during this rollback.
