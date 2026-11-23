
import cython

from ._dns cimport DNSRecord


cdef class RecordUpdate:

    cdef public DNSRecord new
    cdef public DNSRecord old
    cdef public object source

    cdef void _fast_init(self, object new, object old, object source)
