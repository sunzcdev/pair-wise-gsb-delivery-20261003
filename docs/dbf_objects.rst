DBF Objects
===========

Arguments
---------

filename
  The DBF file to open.

  The file name is case insensitive, which means ``DBF('PEOPLE.DBF')``
  will open the file ``people.dbf``. If there is a memo file, it too
  will be looked for in a case insensitive manner, so
  ``DBF('PEOPLE.DBF')`` would find the memo file ``people.FPT``.

  ``DBFNotFound`` will be raised if the file is not found, and
  ``MissingMemoFile`` if the memo file is missing.

load=False
  By default records will streamed directly from disk. If you pass
  ``load=True`` they will instead be loaded into lists and made
  available as the ``records`` and ``deleted`` attributes.

  You can load and unload records at any time with the ``load()`` and
  ``unload()`` methods.

encoding=None
  Specify character encoding to use.

  By default dbfread will try to guess character encoding from the
  ``language_driver`` byte. If this fails it falls back on
  ASCII.

char_decode_errors='strict'

  The error handling scheme to use for the handling of decoding
  errors.  This is passed as the ``errors`` option to the
  ``bytes.decode()`` method. From the documentation of that method:

  "The default is 'strict' meaning that decoding errors raise a
  UnicodeDecodeError. Other possible values are 'ignore' and 'replace'
  as well as any other name registered with codecs.register_error that
  can handle UnicodeDecodeErrors."

lowernames=False
  Field names are typically uppercase. If you pass ``True`` all field
  names will be converted to lowercase.

recfactory=dict (collections.OrderedDict in older versions of Python)

  Takes a function that will be used to produce new records. The
  function will be called with a list of ``(name, value)`` pairs.

  If you pass ``recfactory=None`` you will get the original ``(name,
  value)`` list.

ignorecase=True
  Windows uses a case preserving file system which means
  ``people.dbf`` and ``PEOPLE.DBF`` are the same file. This causes
  problems in for example Linux where case is significant.  To get
  around this dbfread ignores case in file names. You can turn this
  off by passing ``ignorecase=False``.

parserclass=FieldParser
  The parser to use when parsing field values. You can use this to add
  new field types or do custom parsing by subclassing
  ``dbfread.FieldParser``. (See :doc:`field_types`.)

ignore_missing_memofile=False
  If you don't have the memo field you can pass
  ``ignore_missing_memofile=True``. All memo fields will then be
  returned as ``None``, so you at least get the rest of the data.

raw=False
  Returns all data values as byte strings. This can be used for
  debugging or for doing your own decoding.

consistent=False
  Turn on a best-effort consistent view of the DBF file and its memo
  file while they are being read.

  When enabled the identity (device, inode, size and modification time)
  of both files is fixed when the table is opened and re-checked on every
  record and again at the end.  If another program replaces, removes,
  truncates, grows or rewrites either file while it is being read a
  ``DBFChangedError`` is raised and the records read so far must be
  considered incomplete.

  This can not give a true transactional snapshot without co-operation
  from the writer.  In particular, a writer that replaces a file and
  restores its exact size and modification time can not be detected.
  Use ``copy_to_temp=True`` below for stronger isolation.  See
  :doc:`consistent_reads`.

copy_to_temp=False
  Copy the DBF file and its memo file into a private temporary directory
  before reading them.  The identity of each source file is pinned
  before the copy and verified again afterwards, so a replacement or
  truncation during the copy aborts the read with ``DBFChangedError``.
  All reading then happens from the copies, which no other program
  knows about, giving the closest thing to a snapshot dbfread can offer.

  This implies ``consistent=True``.  The temporary directory is removed
  when the table is closed (``close()`` or the end of a ``with`` block)
  or if construction fails.  The original files are opened read-only and
  are never modified or deleted.  See :doc:`consistent_reads`.


Methods
-------

load()
   Load records into memory. This loads both records and deleted
   records. The ``records`` and ``deleted`` attributes will now be
   lists of records.

unload()
   Unload records from memory. The ``records`` and ``deleted``
   attributes will now be instances of ``RecordIterator``, which
   streams records from disk.

close()
   Close the table.  When reading from a temporary copy
   (``copy_to_temp=True``) this also deletes the private temporary
   directory and its copies.  The original DBF and memo files are never
   touched.  After closing, iterating or counting records raises
   ``DBFChangedError``.


Attributes
----------

records
  If the table is loaded this is a list of records. If not, it's a
  ``RecordIterator`` object. In either case, iterating over it or
  calling ``len()`` on it will give the same results.

deleted
  If the table is loaded this is a list of deleted records. If not,
  it's a ``RecordIterator`` object. In either case, iterating over it
  or calling ``len()`` on it will give the same results.

loaded
  ``True`` if records are loaded into memory.

dbversion
  The name of the program that created the database (based on the
  ``dbversion`` byte in the header). Example: ``"FoxBASE+/Dbase III
  plus, no memory"``.

name
  Name of the table. This is the lowercased stem of the filename, for
  example the file ``/home/me/SHOES.dbf`` will have the name
  ``shoes``.

date
  Date when the file was last updated (as ``datetime.date``) or
  ``None`` if the date was all zeroes or invalid.

field_names
  A list of field names in the order they appear in the file. This can
  for example be used to produce the header line in a CSV file.

encoding
  Character encoding used in the file. This is determined by the
  ``language_driver`` byte in the header, and can be overriden with the
  ``encoding`` keyword argument.

ignorecase, lowernames, recfactory, parserclass, raw
  These are set to the values of the same keyword arguments.

filename
  File name of the DBF file.  When ``copy_to_temp=True`` this is the
  private copy in the temporary directory rather than the original.

memofilename
  File name of the memo file, or ``None`` if there is no memo file.
  When ``copy_to_temp=True`` this is the private copy in the temporary
  directory rather than the original.

source_filename
  File name of the original DBF file as passed in (or found with case
  insensitive lookup).  This is the same as ``filename`` unless
  ``copy_to_temp=True`` is used.

source_memofilename
  File name of the original memo file, or ``None`` if there is none.
  This is the same as ``memofilename`` unless ``copy_to_temp=True`` is
  used.

consistent
  ``True`` if the table is doing consistent reads (set by either
  ``consistent=True`` or ``copy_to_temp=True``).

copy_to_temp
  Value of the same keyword argument: ``True`` when reading from a
  private temporary copy.

closed
  ``True`` after ``close()`` has been called.

header
  The file header. This is only intended for internal use, but is exposed
  for debugging purposes. Example::

      DBFHeader(dbversion=3, year=114, month=8, day=2, numrecords=3,
      headerlen=97, recordlen=25, reserved1=0, incomplete_transaction=0,
      encryption_flag=0, free_record_thread=0, reserved2=0, reserved3=0,
      mdx_flag=0, language_driver=0, reserved4=0)

fields
  A list of field headers from the file. Example of a field::

      DBFField(name='NAME', type='C', address=1, length=16, decimal_count=0,
      reserved1=0, workarea_id=0, reserved2=0, reserved3=0, set_fields_flag=0,
      reserved4=b'\x00\x00\x00\x00\x00\x00\x00', index_field_flag=0)

  Only the ``name``, ``type`` and ``length`` attributes are used.
