from __future__ import print_function
import warnings
from .dbf import DBF


class DeprecatedDBF(DBF, list):
    """This is the old version of the table which is a subclass of list.

    It is included for backwards compatability with 1.0 and older."""
    @property
    def loaded(self):
        # Since records are loaded into the table object
        # we have to check the deleted attribute here.
        return isinstance(self._deleted, list)

    def load(self):
        if not self.loaded:
            # Read the file once so active and deleted records keep
            # their order from the file.
            envelopes = list(self._iter_envelopes(report=True))
            self._envelopes = envelopes
            self[:] = [envelope.record for envelope in envelopes
                       if not envelope.deleted]
            self._deleted = [envelope.record for envelope in envelopes
                             if envelope.deleted]

    def unload(self):
        # self.loaded is not checked here because this
        # is called by __init__() where self.loaded=False.
        # Also, unloading twice has no consequences.
        del self[:]
        self._deleted = None
        self._envelopes = None

    def __iter__(self):
        if self.loaded:
            return list.__iter__(self)
        else:
            return self._iter_records()

    def __len__(self):
        if self.loaded:
            return list.__len__(self)
        else:
            return self._count_records()

    def __repr__(self):
        if self.loaded:
            return list.__repr__(self)
        else:
            return '<unloaded DBF table {!r}>'.format(self.filename)


def read(filename, load=True, **kwargs):
    warnings.warn("dbfread.read() has been replaced by DBF(load=True)"
                  " and will be removed in 2.2.")
    return DeprecatedDBF(filename, load=True, **kwargs)


def open(filename, load=True, **kwargs):
    warnings.warn("dbfread.open() has been replaced by DBF()"
                  " and will be removed in 2.2.")
    return DeprecatedDBF(filename, load=True, **kwargs)
