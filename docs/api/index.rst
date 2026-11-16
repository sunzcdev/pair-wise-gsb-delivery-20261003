API Reference
=============

Top-level Functions
-------------------

.. autofunction:: chardet.detect

.. autofunction:: chardet.detect_all

.. autofunction:: chardet.detect_segments

UniversalDetector
-----------------

.. autoclass:: chardet.UniversalDetector
   :members:
   :undoc-members:

Enumerations
------------

.. autoclass:: chardet.EncodingEra
   :members:
   :undoc-members:

.. autoclass:: chardet.LanguageFilter
   :members:
   :undoc-members:

Result Types
------------

.. autoclass:: chardet.DetectionResult
   :members:
   :undoc-members:

.. autoclass:: chardet.DetectionDict
   :members:
   :undoc-members:

.. autoclass:: chardet.Segment
   :members:
   :undoc-members:

.. autoclass:: chardet.EncodingChange
   :members:
   :undoc-members:

.. autoclass:: chardet.SegmentReport
   :members:
   :undoc-members:

Constants
---------

.. py:data:: chardet.DEFAULT_MAX_BYTES
   :type: int
   :value: 200000

   Default maximum number of bytes to examine during detection.

.. py:data:: chardet.MINIMUM_THRESHOLD
   :type: float
   :value: 0.20

   Default minimum confidence threshold for filtering results in
   :func:`chardet.detect_all`.

.. py:data:: chardet.DEFAULT_WINDOW_SIZE
   :type: int
   :value: 16384

   Default window size in bytes for :func:`chardet.detect_segments`.
