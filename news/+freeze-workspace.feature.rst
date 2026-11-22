``pip freeze`` gains a ``--workspace`` option: PEP 610 direct URL references
(editable installs, local directories, archives and VCS URLs) located under
the given workspace directory are written as paths relative to it, so the
output stays valid when the workspace is moved to a different location.
