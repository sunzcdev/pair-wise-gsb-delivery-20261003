import io
import json
import os
import shutil

import pytest

from warcio.indexer import Indexer, ResumeError

from . import get_test_file


FIELDS = 'offset,length,warc-type'

# physical record offsets in the test files
WARC_GZ_OFFSETS = [0, 353, 784, 2012, 2621, 3207]
WARC_OFFSETS = [0, 488, 1197, 2566, 3370, 4316]


class FailAfterWrites(object):
    """ Output which raises IOError after a given number of successful
    writes, simulating a downstream output failure mid-indexing.
    """
    def __init__(self, fail_at):
        self.buff = io.StringIO()
        self.fail_at = fail_at
        self.num_writes = 0

    def write(self, data):
        if self.num_writes >= self.fail_at:
            raise IOError('simulated downstream output failure')
        self.num_writes += 1
        return self.buff.write(data)

    def flush(self):
        pass

    def getvalue(self):
        return self.buff.getvalue()


class NonSeekableWrapper(object):
    def __init__(self, fh):
        self.fh = fh

    def read(self, size=-1):
        return self.fh.read(size)

    def tell(self):
        return self.fh.tell()

    def seekable(self):
        return False


def run_index(filename, output, checkpoint=None):
    indexer = Indexer(FIELDS, [filename], output, checkpoint=checkpoint)
    indexer.process_all()
    return indexer


def full_index(filename):
    out = io.StringIO()
    run_index(filename, out)
    return out.getvalue()


def load_checkpoint(path):
    with open(path, 'r') as fh:
        return json.load(fh)


@pytest.fixture
def checkpoint_path(tmpdir):
    return str(tmpdir.join('checkpoint.json'))


def test_full_run_leaves_checkpoint_at_end(checkpoint_path):
    filename = get_test_file('example.warc.gz')
    out = io.StringIO()
    indexer = run_index(filename, out, checkpoint=checkpoint_path)

    data = load_checkpoint(checkpoint_path)
    state = data['files'][filename]

    # all records consumed: next offset is the physical end of file
    assert state['offset'] == os.path.getsize(filename)
    assert state['compressed'] is True
    assert state['format'] == 'warc'
    assert state['size'] == os.path.getsize(filename)
    assert 'mtime_ns' in state

    # exportable via the API as well
    assert indexer.get_resume_state(filename)['offset'] == state['offset']
    assert indexer.get_resume_state()[filename]['offset'] == state['offset']


def test_checkpoint_offsets_are_physical_gzip_offsets(checkpoint_path):
    filename = get_test_file('example.warc.gz')

    # fail after each record and collect the checkpoint offsets
    offsets = []
    for fail_at in range(1, len(WARC_GZ_OFFSETS)):
        if os.path.exists(checkpoint_path):
            os.remove(checkpoint_path)
        out = FailAfterWrites(fail_at)
        with pytest.raises(IOError):
            run_index(filename, out, checkpoint=checkpoint_path)
        state = load_checkpoint(checkpoint_path)['files'][filename]
        offsets.append(state['offset'])

    # offsets of the *next* record, physical gzip member offsets --
    # a decompressed offset would be much larger
    assert offsets == WARC_GZ_OFFSETS[1:]


def test_resume_after_output_failure_gzip(checkpoint_path):
    filename = get_test_file('example.warc.gz')
    expected = full_index(filename)
    expected_lines = expected.splitlines()

    # fail while writing the 4th record (index 3)
    out = FailAfterWrites(3)
    with pytest.raises(IOError):
        run_index(filename, out, checkpoint=checkpoint_path)

    assert out.getvalue().splitlines() == expected_lines[:3]

    # resume: the failed record must be redone, not skipped
    out2 = io.StringIO()
    run_index(filename, out2, checkpoint=checkpoint_path)

    assert out.getvalue() + out2.getvalue() == expected
    assert '"offset": "2012"' in out2.getvalue()


def test_resume_after_output_failure_plain(checkpoint_path):
    filename = get_test_file('example.warc')
    expected = full_index(filename)
    expected_lines = expected.splitlines()

    out = FailAfterWrites(2)
    with pytest.raises(IOError):
        run_index(filename, out, checkpoint=checkpoint_path)

    assert out.getvalue().splitlines() == expected_lines[:2]

    state = load_checkpoint(checkpoint_path)['files'][filename]
    assert state['compressed'] is False
    # physical offset of 3rd record in the uncompressed file
    assert state['offset'] == WARC_OFFSETS[2]

    out2 = io.StringIO()
    run_index(filename, out2, checkpoint=checkpoint_path)

    assert out.getvalue() + out2.getvalue() == expected


def test_resume_completed_file_produces_no_output(checkpoint_path):
    filename = get_test_file('example.warc.gz')
    run_index(filename, io.StringIO(), checkpoint=checkpoint_path)

    out = io.StringIO()
    run_index(filename, out, checkpoint=checkpoint_path)
    assert out.getvalue() == ''


def test_resume_refused_when_file_truncated(checkpoint_path, tmpdir):
    filename = str(tmpdir.join('trunc.warc.gz'))
    shutil.copy(get_test_file('example.warc.gz'), filename)

    out = FailAfterWrites(3)
    with pytest.raises(IOError):
        run_index(filename, out, checkpoint=checkpoint_path)

    # truncate the source file
    with open(filename, 'r+b') as fh:
        fh.truncate(2000)

    with pytest.raises(ResumeError):
        run_index(filename, io.StringIO(), checkpoint=checkpoint_path)


def test_resume_refused_when_file_replaced(checkpoint_path, tmpdir):
    filename = str(tmpdir.join('replaced.warc'))
    shutil.copy(get_test_file('example.warc'), filename)

    out = FailAfterWrites(2)
    with pytest.raises(IOError):
        run_index(filename, out, checkpoint=checkpoint_path)

    # replace with a different file of the same size class
    shutil.copy(get_test_file('example-iana.org-chunked.warc'), filename)

    with pytest.raises(ResumeError):
        run_index(filename, io.StringIO(), checkpoint=checkpoint_path)


def test_resume_refused_for_non_seekable_input():
    filename = get_test_file('example.warc.gz')
    state = {
        'version': 1,
        'filename': filename,
        'offset': 353,
        'compressed': True,
        'format': 'warc',
    }

    with open(filename, 'rb') as fh:
        input_ = NonSeekableWrapper(fh)
        indexer = Indexer(FIELDS, [filename], io.StringIO())
        with pytest.raises(ResumeError):
            indexer.process_one(input_, io.StringIO(), filename,
                                resume_state=state)


def test_resume_state_matches_full_run():
    # resuming from each record boundary must reproduce the full index
    filename = get_test_file('example.warc.gz')
    expected = full_index(filename)

    for offset in WARC_GZ_OFFSETS:
        state = {
            'version': 1,
            'filename': filename,
            'offset': offset,
            'compressed': True,
            'format': 'warc',
        }
        out = io.StringIO()
        with open(filename, 'rb') as fh:
            indexer = Indexer(FIELDS, [filename], out)
            indexer.process_one(fh, out, filename, resume_state=state)

        remaining = expected
        if offset > 0:
            # lines before the resume offset are not re-emitted
            count = WARC_GZ_OFFSETS.index(offset)
            remaining = ''.join(expected.splitlines(keepends=True)[count:])
        assert out.getvalue() == remaining


def test_cli_index_with_checkpoint(capsys, checkpoint_path):
    from warcio.cli import main

    filename = get_test_file('example.warc.gz')

    main(args=['index', '-f', FIELDS, '--checkpoint', checkpoint_path,
               filename])
    full = capsys.readouterr().out
    assert os.path.exists(checkpoint_path)

    # second run resumes at end of file: no additional output
    main(args=['index', '-f', FIELDS, '--checkpoint', checkpoint_path,
               filename])
    assert capsys.readouterr().out == ''

    # without the checkpoint, full output is produced again
    main(args=['index', '-f', FIELDS, filename])
    assert capsys.readouterr().out == full
