import json
import sys
import os
import time

from collections import OrderedDict

from warcio.archiveiterator import ArchiveIterator
from warcio.utils import fsspec_open


# ============================================================================
class ResumeError(Exception):
    """ Raised when an indexing resume/checkpoint position can not be
    honored, e.g. the source file was replaced or truncated, or the
    input does not support seeking.
    """
    pass


# ============================================================================
class Indexer(object):
    field_names = {}

    GZIP_MAGIC = b'\x1f\x8b'

    RESUME_STATE_VERSION = 1

    def __init__(self, fields, inputs, output, verify_http=False,
                 checkpoint=None):
        if isinstance(fields, str):
            fields = fields.split(',')
        self.fields = fields
        self.record_parse = any(field.startswith('http:') for field in self.fields)

        self.inputs = inputs
        self.output = output
        self.verify_http = verify_http

        # optional path to a checkpoint file used to persist and
        # resume the indexing position
        self.checkpoint_path = checkpoint

        # most recent exported resume state (per filename)
        self.resume_states = {}

    def process_all(self):
        saved_states = self._load_checkpoint()
        with fsspec_open(self.output, 'wt', sys.stdout) as out:
            for filename in self.inputs:
                try:
                    stdin = sys.stdin.buffer
                except AttributeError:  # py2
                    stdin = sys.stdin
                with fsspec_open(filename, 'rb', stdin) as fh:
                    self.process_one(fh, out, filename,
                                     resume_state=saved_states.get(filename))

    def process_one(self, input_, output, filename, resume_state=None):
        start_offset = 0
        if resume_state:
            start_offset = self._seek_to_resume_offset(input_, filename,
                                                       resume_state)

        it = self._create_record_iter(input_)

        if start_offset == 0:
            self._write_header(output, filename)

        for record in it:
            self.process_index_entry(it, record, filename, output)

    def process_index_entry(self, it, record, filename, output):
        index = self._new_dict(record)

        for field in self.fields:
            value = self.get_field(record, field, it, filename)

            if value is not None:
                field = self.field_names.get(field, field)
                index[field] = value

        # fully consume the record before writing the index line, so that
        # the physical offset of the *next* record is known and the record
        # never needs to be re-read to complete this entry
        it.read_to_end()

        self._write_line(output, index, record, filename)

        # only advance the resume position after the record has been fully
        # consumed AND the index line has been successfully written;
        # if the write above fails, the resume position still points at
        # this record and it will be redone on resume
        self._advance_resume_state(it, filename, output)

    def get_resume_state(self, filename=None):
        """ Return the current exportable resume position(s).

        If filename is given, return the resume state dict for that input
        (or None). Otherwise return a dict of all known resume states,
        keyed by input filename.
        """
        if filename is not None:
            return self.resume_states.get(filename)
        return dict(self.resume_states)

    # ========================================================================
    # resume / checkpoint support
    # ========================================================================
    def _advance_resume_state(self, it, filename, output):
        state = self._make_resume_state(it, filename)
        self.resume_states[filename] = state

        if self.checkpoint_path:
            # make sure the index line actually reaches the output before
            # the resume position past it is persisted
            try:
                output.flush()
            except (AttributeError, IOError):
                pass
            self._save_checkpoint(filename, state)

    def _make_resume_state(self, it, filename):
        # it.offset is the *physical* file offset of the next record:
        # for plain files it is the byte offset of the next record, for
        # per-record gzip files it is the offset of the next gzip member.
        # It is never a decompressed offset.
        format_ = it.known_format
        if not format_ and it.record is not None:
            format_ = getattr(it.record, 'format', None)

        state = {
            'version': self.RESUME_STATE_VERSION,
            'filename': filename,
            'offset': it.offset,
            'compressed': it.reader is not None and it.reader.decompressor is not None,
            'format': format_,
        }

        stat = self._stat_input(it.fh, filename)
        if stat:
            state['size'] = stat.st_size
            state['mtime_ns'] = getattr(stat, 'st_mtime_ns', None) or \
                int(stat.st_mtime * 1e9)

        return state

    @staticmethod
    def _stat_input(input_, filename):
        try:
            return os.fstat(input_.fileno())
        except (AttributeError, IOError, OSError):
            pass

        if isinstance(filename, str) and filename != '-':
            try:
                return os.stat(filename)
            except (IOError, OSError):
                pass

        return None

    def _seek_to_resume_offset(self, input_, filename, resume_state):
        """ Validate the resume state against the current input and seek
        to the stored physical offset. Raises ResumeError if the input
        can not be safely resumed.
        """
        offset = resume_state.get('offset', 0)
        if not offset:
            return 0

        if resume_state.get('filename') != filename:
            raise ResumeError(
                'Resume state does not match input: {0} != {1}'.format(
                    resume_state.get('filename'), filename))

        # inputs which do not support seeking can not be resumed
        seekable = False
        try:
            seekable = input_.seekable()
        except (AttributeError, IOError, OSError):
            seekable = False

        if not seekable:
            raise ResumeError(
                'Can not resume indexing of {0}: '
                'input does not support seeking'.format(filename))

        # verify the source file has not been replaced or truncated
        stat = self._stat_input(input_, filename)
        if stat is None:
            raise ResumeError(
                'Can not resume indexing of {0}: '
                'unable to verify source file identity'.format(filename))

        size = resume_state.get('size')
        if size is not None and stat.st_size != size:
            raise ResumeError(
                'Can not resume indexing of {0}: source file was replaced '
                'or truncated (size {1} != expected {2})'.format(
                    filename, stat.st_size, size))

        mtime_ns = resume_state.get('mtime_ns')
        curr_mtime_ns = getattr(stat, 'st_mtime_ns', None) or \
            int(stat.st_mtime * 1e9)
        if mtime_ns is not None and curr_mtime_ns != mtime_ns:
            raise ResumeError(
                'Can not resume indexing of {0}: source file was '
                'modified since checkpoint'.format(filename))

        if offset > stat.st_size:
            raise ResumeError(
                'Can not resume indexing of {0}: source file was truncated '
                '(offset {1} beyond size {2})'.format(
                    filename, offset, stat.st_size))

        # verify the format info matches the data at the resume offset
        if offset < stat.st_size:
            input_.seek(offset)
            magic = input_.read(len(self.GZIP_MAGIC))
            is_gzip = magic == self.GZIP_MAGIC
            if bool(resume_state.get('compressed')) != is_gzip:
                raise ResumeError(
                    'Can not resume indexing of {0}: data at offset {1} '
                    'does not match checkpoint format info'.format(
                        filename, offset))

        input_.seek(offset)
        return offset

    def _load_checkpoint(self):
        if not self.checkpoint_path or not os.path.exists(self.checkpoint_path):
            return {}

        try:
            with open(self.checkpoint_path, 'r') as fh:
                data = json.load(fh)
        except ValueError:
            return {}

        if data.get('version') != self.RESUME_STATE_VERSION:
            return {}

        return data.get('files', {})

    def _save_checkpoint(self, filename, state):
        states = {}
        if os.path.exists(self.checkpoint_path):
            try:
                with open(self.checkpoint_path, 'r') as fh:
                    states = json.load(fh).get('files', {})
            except ValueError:
                states = {}

        states[filename] = state

        # write atomically so an interrupted checkpoint write does not
        # corrupt the last good resume position
        tmp_path = self.checkpoint_path + '.tmp'
        with open(tmp_path, 'w') as fh:
            json.dump({'version': self.RESUME_STATE_VERSION,
                       'files': states}, fh)

        # os.replace may transiently fail on some platforms (e.g. a
        # virus scanner briefly locking the file), so retry a few times
        # before falling back to a direct write
        for attempt in range(3):
            try:
                os.replace(tmp_path, self.checkpoint_path)
                return
            except OSError:
                if attempt == 2:
                    break
                time.sleep(0.05)

        try:
            with open(self.checkpoint_path, 'w') as fh:
                json.dump({'version': self.RESUME_STATE_VERSION,
                           'files': states}, fh)
        finally:
            try:
                os.remove(tmp_path)
            except OSError:
                pass

    def _create_record_iter(self, input_):
        return ArchiveIterator(input_,
                               no_record_parse=not self.record_parse,
                               arc2warc=True,
                               verify_http=self.verify_http)

    def _new_dict(self, record):
        return OrderedDict()

    def get_field(self, record, name, it, filename):
        value = None
        if name == 'offset':
            value = str(it.get_record_offset())
        elif name == 'length':
            value = str(it.get_record_length())
        elif name == 'filename':
            value = os.path.basename(filename)
        elif name == 'http:status':
            if record.rec_type in ('response', 'revisit') and record.http_headers:
                value = record.http_headers.get_statuscode()
        elif name.startswith('http:'):
            if record.http_headers:
                value = record.http_headers.get_header(name[5:])
        else:
            value = record.rec_headers.get_header(name)

        return value

    def _write_header(self, out, filename):
        pass

    def _write_line(self, out, index, record, filename):
        out.write(json.dumps(index) + '\n')


