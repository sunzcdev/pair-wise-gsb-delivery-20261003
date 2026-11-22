.. _programmatic_api:

########
 Python
########

The primary interface to ``virtualenv`` is the command line application. However, it can also be used programmatically
via the ``virtualenv.cli_run`` function and the ``Session`` class.

See :doc:`../how-to/usage` for usage examples.

*******************
 virtualenv module
*******************

.. automodule:: virtualenv
    :members:

***************
 Session class
***************

The ``Session`` class represents a virtualenv creation session and provides access to the created environment's
properties.

.. currentmodule:: virtualenv.run.session

.. autoclass:: Session
    :members:

*******************
 VirtualEnvOptions
*******************

Options namespace passed to plugin constructors, populated from the CLI, environment variables, and configuration files.

.. currentmodule:: virtualenv.config.cli.parser

.. autoclass:: VirtualEnvOptions
    :members:

***************
 Configuration
***************

``audit_via_cli`` runs the same parse the creation runs, so the values it reports are the values the creation would
read. Use it to explain how the config file, the environment variables and the command line resolved an option.

.. currentmodule:: virtualenv.run

.. autofunction:: audit_via_cli

.. currentmodule:: virtualenv.config.audit

.. autoclass:: ConfigAudit
    :members:

.. autoclass:: OptionAudit
    :members:

.. autoclass:: LayerReading
    :members:

