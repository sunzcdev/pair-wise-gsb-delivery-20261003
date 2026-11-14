import pytest

from io import BytesIO

from warcio.archiveiterator import ArchiveIterator
from warcio.cli import main
from warcio.digestverifyingreader import DigestChecker, _compare_digest_rfc_3548
from warcio.exceptions import ArchiveLoadFailed
from warcio.statusandheaders import StatusAndHeaders
from warcio.utils import Digester
from warcio.warcwriter import BufferWARCWriter

from . import get_test_file


empty_sha1_b32 = '3I42H3S6NNFQ2MSVX7XZKYAYSCX5QBYJ'
empty_sha1_b64 = '2jmj7l5rSw0yVb/vlWAYkK/YBwk='
empty_sha1_b64_alt = '2jmj7l5rSw0yVb_vlWAYkK_YBwk='
empty_sha1_b16 = 'DA39A3EE5E6B4B0D3255BFEF95601890AFD80709'


def test_compare_digest_rfc_3548():
    assert _compare_digest_rfc_3548(None, None) is None
    sha1d = Digester('sha1')
    assert _compare_digest_rfc_3548(sha1d, 'sha1:'+empty_sha1_b32) is True
    assert _compare_digest_rfc_3548(sha1d, 'sha1:'+empty_sha1_b32.replace('3I', 'xx')) is False

    assert _compare_digest_rfc_3548(sha1d, 'sha1:'+empty_sha1_b64) is True
    assert _compare_digest_rfc_3548(sha1d, 'sha1:'+empty_sha1_b64_alt) is True
    assert _compare_digest_rfc_3548(sha1d, 'sha1:'+empty_sha1_b16) is True
    assert _compare_digest_rfc_3548(sha1d, 'sha1:'+empty_sha1_b16.lower()) is True

    with pytest.raises(ValueError):
        assert _compare_digest_rfc_3548(sha1d, 'foo') is False
        assert _compare_digest_rfc_3548(sha1d, 'foo:bar') is False


# ============================================================================
def _make_response_warc():
    payload = b'some\ntext'

    headers_list = [('Content-Type', 'text/plain; charset="UTF-8"'),
                    ('Content-Length', str(len(payload)))]

    http_headers = StatusAndHeaders('200 OK', headers_list, protocol='HTTP/1.0')

    writer = BufferWARCWriter(gzip=False)
    record = writer.create_warc_record('http://example.com/', 'response',
                                       payload=BytesIO(payload),
                                       length=len(payload),
                                       http_headers=http_headers)
    writer.write_record(record)
    return writer.get_contents()


REVISIT_WARC = (
    b'WARC/1.0\r\n'
    b'WARC-Type: revisit\r\n'
    b'WARC-Record-ID: <urn:uuid:12345678-1234-1234-1234-123456789012>\r\n'
    b'WARC-Target-URI: http://example.com/\r\n'
    b'WARC-Date: 2017-03-06T04:02:06Z\r\n'
    b'WARC-Block-Digest: sha1:3I42H3S6NNFQ2MSVX7XZKYAYSCX5QBYJ\r\n'
    b'WARC-Payload-Digest: sha1:3I42H3S6NNFQ2MSVX7XZKYAYSCX5QBYJ\r\n'
    b'Content-Type: application/http; msgtype=response\r\n'
    b'Content-Length: 0\r\n'
    b'\r\n'
    b'\r\n'
)


# ============================================================================
class TestPerDigestStatus(object):
    def test_checker_default_statuses(self):
        checker = DigestChecker()
        assert checker.block_status == 'not_declared'
        assert checker.payload_status == 'not_declared'
        assert checker.passed is None
        assert checker.status == 'not_checked'
        assert checker.problems == []

    def test_checker_pass_does_not_mask_pending(self):
        checker = DigestChecker()
        checker.block_status = DigestChecker.STATUS_PASSED
        checker.payload_status = DigestChecker.STATUS_PENDING
        assert checker.passed is None
        assert checker.status == 'pending'

    def test_checker_pass_does_not_mask_failure(self):
        checker = DigestChecker()
        checker.block_status = DigestChecker.STATUS_PASSED
        checker.payload_status = DigestChecker.STATUS_FAILED
        assert checker.passed is False
        assert checker.status == 'failed'

    def test_checker_all_passed(self):
        checker = DigestChecker()
        checker.block_status = DigestChecker.STATUS_PASSED
        checker.payload_status = DigestChecker.STATUS_PASSED
        assert checker.passed is True
        assert checker.status == 'passed'

    def test_checker_unsupported_is_not_a_pass(self):
        checker = DigestChecker()
        checker.block_status = DigestChecker.STATUS_UNSUPPORTED
        checker.payload_status = DigestChecker.STATUS_PASSED
        assert checker.passed is False
        assert checker.status == 'failed'

    def test_checker_not_applicable(self):
        checker = DigestChecker()
        checker.block_status = DigestChecker.STATUS_PASSED
        checker.payload_status = DigestChecker.STATUS_NOT_APPLICABLE
        assert checker.passed is True
        assert checker.status == 'passed'

        checker.block_status = DigestChecker.STATUS_NOT_APPLICABLE
        checker.payload_status = DigestChecker.STATUS_NOT_APPLICABLE
        assert checker.passed is None
        assert checker.status == 'not_checked'

    def test_checker_passed_setter_compat(self):
        checker = DigestChecker()
        checker.passed = True
        assert checker.passed is True
        checker.passed = False
        assert checker.passed is False
        checker.passed = None
        assert checker.passed is None

    def test_no_digests_declared(self):
        contents = _make_response_warc()
        contents = contents.replace(b'WARC-Block-Digest:', b'X-Block-Digest:')
        contents = contents.replace(b'WARC-Payload-Digest:', b'X-Payload-Digest:')

        it = ArchiveIterator(BytesIO(contents), check_digests=True)
        record = next(it)
        it.read_to_end()

        checker = record.digest_checker
        assert checker.block_status == 'not_declared'
        assert checker.payload_status == 'not_declared'
        assert checker.passed is None
        assert checker.status == 'not_checked'

    def test_statuses_pending_until_bytes_read(self):
        it = ArchiveIterator(BytesIO(_make_response_warc()), check_digests=True)
        record = next(it)

        checker = record.digest_checker
        # http headers already consumed, but payload not read yet
        assert checker.block_status == 'pending'
        assert checker.payload_status == 'pending'
        assert checker.passed is None
        assert checker.status == 'pending'

        # partial read: still pending, nothing finalized
        buff = record.content_stream().read(4)
        assert len(buff) == 4
        assert checker.block_status == 'pending'
        assert checker.payload_status == 'pending'
        assert checker.passed is None

        # skipping the remainder via the iterator finalizes the statuses
        it.read_to_end()
        assert checker.block_status == 'passed'
        assert checker.payload_status == 'passed'
        assert checker.passed is True
        assert checker.status == 'passed'

    def test_explicit_full_read_finalizes(self):
        it = ArchiveIterator(BytesIO(_make_response_warc()), check_digests=True)
        record = next(it)

        checker = record.digest_checker
        assert checker.passed is None

        while record.content_stream().read(3):
            pass

        assert checker.block_status == 'passed'
        assert checker.payload_status == 'passed'
        assert checker.passed is True
        assert checker.status == 'passed'

    def test_failure_not_masked_by_pass(self):
        # first record of example-digest.warc: valid block digest,
        # invalid payload digest
        with open(get_test_file('example-digest.warc'), 'rb') as fh:
            it = ArchiveIterator(fh, check_digests=True)
            record = next(it)
            it.read_to_end()

            checker = record.digest_checker
            assert checker.block_status == 'passed'
            assert checker.payload_status == 'failed'
            assert checker.passed is False
            assert checker.status == 'failed'
            assert len(checker.problems) == 1

    def test_eof_reread_does_not_double_register(self):
        with open(get_test_file('example-digest.warc'), 'rb') as fh:
            it = ArchiveIterator(fh, check_digests=True)
            record = next(it)
            it.read_to_end()

            checker = record.digest_checker
            assert checker.payload_status == 'failed'
            assert len(checker.problems) == 1

            # repeated reads at EOF must not register the same failure again
            assert record.raw_stream.read() == b''
            assert record.raw_stream.read(10) == b''
            assert record.raw_stream.readline() == b''
            assert len(checker.problems) == 1
            assert checker.payload_status == 'failed'
            assert checker.passed is False

    def test_unsupported_algorithm(self):
        contents = _make_response_warc()
        contents = contents.replace(b'WARC-Block-Digest: sha1:',
                                    b'WARC-Block-Digest: foo:', 1)
        assert b'WARC-Block-Digest: foo:' in contents

        it = ArchiveIterator(BytesIO(contents), check_digests=True)
        record = next(it)
        it.read_to_end()

        checker = record.digest_checker
        # unsupported algorithm must not count as a pass
        assert checker.block_status == 'unsupported'
        assert checker.payload_status == 'passed'
        assert checker.passed is False
        assert checker.status == 'failed'
        assert checker.problems == ['unknown hash algorithm name in block digest']

    def test_unsupported_algorithm_log(self, capsys):
        contents = _make_response_warc()
        contents = contents.replace(b'WARC-Payload-Digest: sha1:',
                                    b'WARC-Payload-Digest: foo:', 1)

        it = ArchiveIterator(BytesIO(contents), check_digests='log')
        record = next(it)
        it.read_to_end()

        out, err = capsys.readouterr()
        assert 'unknown hash algorithm name in payload digest' in err

        checker = record.digest_checker
        assert checker.block_status == 'passed'
        assert checker.payload_status == 'unsupported'
        assert checker.passed is False

    def test_unsupported_algorithm_raise(self):
        contents = _make_response_warc()
        contents = contents.replace(b'WARC-Block-Digest: sha1:',
                                    b'WARC-Block-Digest: foo:', 1)

        it = ArchiveIterator(BytesIO(contents), check_digests='raise')
        with pytest.raises(ArchiveLoadFailed):
            next(it)

    def test_revisit_digests_not_applicable(self):
        it = ArchiveIterator(BytesIO(REVISIT_WARC), check_digests=True)
        record = next(it)
        it.read_to_end()

        checker = record.digest_checker
        assert checker.block_status == 'not_applicable'
        assert checker.payload_status == 'not_applicable'
        assert checker.passed is None
        assert checker.status == 'not_checked'
        assert checker.problems == []

    def test_cli_verbose_per_digest_status(self, capsys):
        exit_value = None
        try:
            main(args=['check', '-v', get_test_file('example-digest.warc')])
        except SystemExit as e:
            exit_value = e.code
        assert exit_value == 1

        out = capsys.readouterr().out
        assert 'block digest: passed, payload digest: failed' in out
        assert 'block digest: passed, payload digest: passed' in out
