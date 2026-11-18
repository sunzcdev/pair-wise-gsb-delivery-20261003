h2 API
======

This document details the API of h2.

Semantic Versioning
-------------------

h2 follows semantic versioning for its public API. Please note that the
guarantees of semantic versioning apply only to the API that is *documented
here*. Simply because a method or data field is not prefaced by an underscore
does not make it part of h2's public API. Anything not documented here is
subject to change at any time.

Connection
----------

.. autoclass:: h2.connection.H2Connection
   :members:
   :exclude-members: inbound_flow_control_window


Configuration
-------------

.. autoclass:: h2.config.H2Configuration
   :members:


.. _h2-events-api:

Events
------

.. autoclass:: h2.events.RequestReceived
   :members:

.. autoclass:: h2.events.ResponseReceived
   :members:

.. autoclass:: h2.events.TrailersReceived
   :members:

.. autoclass:: h2.events.InformationalResponseReceived
   :members:

.. autoclass:: h2.events.DataReceived
   :members:

.. autoclass:: h2.events.WindowUpdated
   :members:

.. autoclass:: h2.events.RemoteSettingsChanged
   :members:

.. autoclass:: h2.events.PingReceived
   :members:

.. autoclass:: h2.events.PingAckReceived
   :members:

.. autoclass:: h2.events.StreamEnded
   :members:

.. autoclass:: h2.events.StreamReset
   :members:

.. autoclass:: h2.events.PushedStreamReceived
   :members:

.. autoclass:: h2.events.SettingsAcknowledged
   :members:

.. autoclass:: h2.events.PriorityUpdated
   :members:

.. autoclass:: h2.events.ConnectionTerminated
   :members:

.. autoclass:: h2.events.AlternativeServiceAvailable
   :members:

.. autoclass:: h2.events.UnknownFrameReceived
   :members:


Alternative Services
--------------------

The ALTSVC field value carried by an :class:`h2.events.AlternativeServiceAvailable`
event is parsed by :func:`h2.alternative_service.parse_alternative_service_field`.
The result is descriptive only: it never establishes a connection and never
decides that an advertised origin or address is trusted.

.. automodule:: h2.alternative_service
   :members: parse_alternative_service_field, parse_origin, service_is_cross_origin,
             validate_advertisement_scope, validate_connection_level_advertisement,
             validate_stream_level_advertisement

.. autoclass:: h2.alternative_service.AlternativeServiceField
   :members:

.. autoclass:: h2.alternative_service.AlternativeService
   :members:

.. autoclass:: h2.alternative_service.AlternativeServiceScope
   :members:

.. autoclass:: h2.alternative_service.OriginSource
   :members:

.. autoclass:: h2.alternative_service.MaxAgeStatus
   :members:

.. autoclass:: h2.alternative_service.InvalidAlternativeServiceAdvertisement
   :show-inheritance:


Exceptions
----------

.. autoclass:: h2.exceptions.H2Error
   :members:

.. autoclass:: h2.exceptions.NoSuchStreamError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.StreamClosedError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.RFC1122Error
   :show-inheritance:
   :members:


Protocol Errors
~~~~~~~~~~~~~~~

.. autoclass:: h2.exceptions.ProtocolError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.FrameTooLargeError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.FrameDataMissingError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.TooManyStreamsError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.FlowControlError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.StreamIDTooLowError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.InvalidSettingsValueError
   :members:

.. autoclass:: h2.exceptions.NoAvailableStreamIDError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.InvalidBodyLengthError
   :show-inheritance:
   :members:

.. autoclass:: h2.exceptions.UnsupportedFrameError
   :members:

.. autoclass:: h2.exceptions.DenialOfServiceError
   :show-inheritance:
   :members:


HTTP/2 Error Codes
------------------

.. automodule:: h2.errors
   :members:


Settings
--------

.. autoclass:: h2.settings.SettingCodes
   :members:

.. autoclass:: h2.settings.Settings
   :inherited-members:

.. autoclass:: h2.settings.ChangedSetting
   :members:
