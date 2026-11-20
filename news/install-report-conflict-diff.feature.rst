Report how an installation changes broken dependency relations.

``pip install --report`` now includes a ``conflicts`` object comparing the
dependency graph before and after the installation. It lists the relations that
the installation introduces, the ones it resolves, and the ones that were
already broken, so an upgrade of an environment with pre-existing conflicts can
be told apart from one that creates new ones. Extras and environment markers are
evaluated with the same rules ``pip check`` uses.
