================================================
 Message Serialization - ``kombu.serialization``
================================================

.. currentmodule:: kombu.serialization

.. automodule:: kombu.serialization

    .. contents::
        :local:

    Overview
    --------

    Centralized support for encoding/decoding of data structures.
    Contains json, pickle, msgpack, and yaml serializers.

    Optionally installs support for YAML if the `PyYAML`_ package
    is installed.

    Optionally installs support for `msgpack`_ if the `msgpack-python`_
    package is installed.


    Exceptions
    ----------

    .. autoexception:: SerializerNotInstalled

    Serialization
    -------------

    .. autofunction:: dumps

    .. autofunction:: loads

    .. autofunction:: raw_encode

    Registry
    --------

    .. autofunction:: register
    .. autofunction:: unregister

    .. autodata:: registry

    Frozen decoder capabilities
    ---------------------------

    .. autoclass:: DecoderView

        .. autoattribute:: accept
        .. autoattribute:: decoders
        .. autoattribute:: content_types

        .. automethod:: resolve_content_type
        .. automethod:: check_content_type
        .. automethod:: decoder_for
        .. automethod:: decode
        .. automethod:: with_accept
        .. automethod:: from_registry

    .. autofunction:: decoder_view

.. _`Python 2.7+`: https://docs.python.org/library/json.html
.. _`PyYAML`: https://pyyaml.org/
.. _`msgpack`: https://msgpack.org/
.. _`msgpack-python`: https://pypi.org/project/msgpack-python/
