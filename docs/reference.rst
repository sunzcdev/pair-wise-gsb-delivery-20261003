.. _reference:

===============
 API reference
===============

.. contents:: :local:
   :class: this-will-duplicate-information-and-it-is-still-useful-here

.. _reference_db_database:

sqlite_utils.db.Database
========================

.. autoclass:: sqlite_utils.db.Database
    :members:
    :undoc-members:
    :special-members: __getitem__
    :exclude-members: use_counts_table, execute_returning_dicts, resolve_foreign_keys

.. _reference_db_queryable:

sqlite_utils.db.Queryable
=========================

:ref:`Table <reference_db_table>` and :ref:`View <reference_db_view>` are  both subclasses of ``Queryable``, providing access to the following methods:

.. autoclass:: sqlite_utils.db.Queryable
    :members:
    :undoc-members:
    :exclude-members: execute_count

.. _reference_db_table:

sqlite_utils.db.Table
=====================

.. autoclass:: sqlite_utils.db.Table
    :members:
    :undoc-members:
    :show-inheritance:
    :exclude-members: guess_foreign_column, value_or_default, build_insert_queries_and_params, insert_chunk, add_missing_columns

.. _reference_db_view:

sqlite_utils.db.View
====================

.. autoclass:: sqlite_utils.db.View
    :members:
    :undoc-members:
    :show-inheritance:

.. _reference_db_other:

Other
=====

.. _reference_db_other_column:

sqlite_utils.db.Column
----------------------

.. autoclass:: sqlite_utils.db.Column

.. _reference_db_other_column_details:

sqlite_utils.db.ColumnDetails
-----------------------------

.. autoclass:: sqlite_utils.db.ColumnDetails

.. _reference_db_other_foreign_key:

sqlite_utils.db.ForeignKey
--------------------------

.. autoclass:: sqlite_utils.db.ForeignKey

sqlite_utils.schema_compat
==========================

.. _reference_schema_compat_check:

sqlite_utils.schema_compat.check_schema_compatibility
-----------------------------------------------------

.. autofunction:: sqlite_utils.schema_compat.check_schema_compatibility

.. _reference_schema_compat_report:

sqlite_utils.schema_compat.SchemaCompatibilityReport
----------------------------------------------------

.. autoclass:: sqlite_utils.schema_compat.SchemaCompatibilityReport
   :members: findings, compatible_tables, findings_by_level, conversion_required, incompatible, unknown, verdict

.. _reference_schema_compat_finding:

sqlite_utils.schema_compat.Finding
----------------------------------

.. autoclass:: sqlite_utils.schema_compat.Finding

.. _reference_schema_compat_levels:

sqlite_utils.schema_compat.CompatibilityLevel
---------------------------------------------

.. autoclass:: sqlite_utils.schema_compat.CompatibilityLevel
   :members:

.. _reference_schema_compat_object_type:

sqlite_utils.schema_compat.ObjectType
-------------------------------------

.. autoclass:: sqlite_utils.schema_compat.ObjectType
   :members:
sqlite_utils.utils
==================

.. _reference_utils_hash_record:

sqlite_utils.utils.hash_record
------------------------------

.. autofunction:: sqlite_utils.utils.hash_record

.. _reference_utils_rows_from_file:

sqlite_utils.utils.rows_from_file
---------------------------------

.. autofunction:: sqlite_utils.utils.rows_from_file

.. _reference_utils_typetracker:

sqlite_utils.utils.TypeTracker
------------------------------

.. autoclass:: sqlite_utils.utils.TypeTracker
   :members: wrap, types

.. _reference_utils_chunks:

sqlite_utils.utils.chunks
-------------------------

.. autofunction:: sqlite_utils.utils.chunks

.. _reference_utils_flatten:

sqlite_utils.utils.flatten
--------------------------

.. autofunction:: sqlite_utils.utils.flatten
