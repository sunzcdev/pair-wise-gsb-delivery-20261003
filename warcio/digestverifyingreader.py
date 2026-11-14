import base64
import sys

from warcio.limitreader import LimitReader
from warcio.utils import to_native_str, Digester
from warcio.exceptions import ArchiveLoadFailed


# ============================================================================
class DigestChecker(object):
    """Tracks the verification status of each digest a record may declare.

    Each digest (block and payload) has its own status, one of:

    - ``STATUS_NOT_DECLARED``: the record did not declare this digest
    - ``STATUS_UNSUPPORTED``: declared, but the hash algorithm is not
      supported (never counts as a pass)
    - ``STATUS_PENDING``: declared, but the corresponding record bytes
      have not been (fully) read yet
    - ``STATUS_PASSED``: declared and verified successfully
    - ``STATUS_FAILED``: declared and did not match
    - ``STATUS_NOT_APPLICABLE``: declared, but not applicable to this
      record (e.g. the payload digest of a revisit record or of a
      record segment)
    """

    STATUS_NOT_DECLARED = 'not_declared'
    STATUS_UNSUPPORTED = 'unsupported'
    STATUS_PENDING = 'pending'
    STATUS_PASSED = 'passed'
    STATUS_FAILED = 'failed'
    STATUS_NOT_APPLICABLE = 'not_applicable'

    STATUS_NOT_CHECKED = 'not_checked'

    def __init__(self, kind=None):
        self._problem = []
        self._passed = None
        self.kind = kind
        self.block_status = self.STATUS_NOT_DECLARED
        self.payload_status = self.STATUS_NOT_DECLARED

    @property
    def passed(self):
        """Overall digest verification result.

        - ``False`` if any digest failed or uses an unsupported algorithm
        - ``None`` if any digest is still pending, or no digest was checked
        - ``True`` only if every declared digest was actually checked and
          passed -- one passing digest never masks another digest which
          failed or is still pending
        """
        status = self.status
        if status == self.STATUS_FAILED:
            return False
        if status == self.STATUS_PASSED:
            return True
        return self._passed

    @passed.setter
    def passed(self, value):
        self._passed = value

    @property
    def status(self):
        """Overall result as a string: one of ``'passed'``, ``'failed'``,
        ``'pending'`` or ``'not_checked'``.
        """
        statuses = (self.block_status, self.payload_status)
        if self.STATUS_FAILED in statuses or self.STATUS_UNSUPPORTED in statuses:
            return self.STATUS_FAILED
        if self.STATUS_PENDING in statuses:
            return self.STATUS_PENDING
        if self.STATUS_PASSED in statuses:
            return self.STATUS_PASSED
        return self.STATUS_NOT_CHECKED

    @property
    def problems(self):
        return self._problem

    def problem(self, value, passed=False):
        self._problem.append(value)
        if self.kind == 'raise':
            raise ArchiveLoadFailed(value)
        if self.kind == 'log':
            sys.stderr.write(value + '\n')
        self._passed = passed


# ============================================================================
class DigestVerifyingReader(LimitReader):
    """
    A reader which verifies the digest of the wrapped reader
    """

    def __init__(self, stream, limit, digest_checker, record_type=None,
                 payload_digest=None, block_digest=None, segment_number=None):

        super(DigestVerifyingReader, self).__init__(stream, limit)

        self.digest_checker = digest_checker

        block_applicable = True
        payload_applicable = True

        if record_type == 'revisit':
            block_applicable = False
            payload_applicable = False
        if segment_number is not None:  #pragma: no cover
            payload_applicable = False

        self.payload_digest = payload_digest
        self.block_digest = block_digest

        self.payload_digester = None
        self.payload_digester_obj = None
        self.block_digester = None

        if not block_digest:
            self.digest_checker.block_status = DigestChecker.STATUS_NOT_DECLARED
        elif not block_applicable:
            self.digest_checker.block_status = DigestChecker.STATUS_NOT_APPLICABLE
        else:
            try:
                algo, _ = _parse_digest(block_digest)
                self.block_digester = Digester(algo)
                self.digest_checker.block_status = DigestChecker.STATUS_PENDING
            except ValueError:
                self.digest_checker.block_status = DigestChecker.STATUS_UNSUPPORTED
                self.digest_checker.problem('unknown hash algorithm name in block digest')

        if not payload_digest:
            self.digest_checker.payload_status = DigestChecker.STATUS_NOT_DECLARED
        elif not payload_applicable:
            self.digest_checker.payload_status = DigestChecker.STATUS_NOT_APPLICABLE
        else:
            try:
                algo, _ = _parse_digest(payload_digest)
                self.payload_digester_obj = Digester(algo)
                self.digest_checker.payload_status = DigestChecker.STATUS_PENDING
            except ValueError:
                self.digest_checker.payload_status = DigestChecker.STATUS_UNSUPPORTED
                self.digest_checker.problem('unknown hash algorithm name in payload digest')

    def begin_payload(self):
        self.payload_digester = self.payload_digester_obj
        if self.limit == 0:
            self._check_payload_digest()

    def _update(self, buff):
        super(DigestVerifyingReader, self)._update(buff)

        if self.payload_digester:
            self.payload_digester.update(buff)
        if self.block_digester:
            self.block_digester.update(buff)

        if self.limit == 0:
            self._check_block_digest()
            self._check_payload_digest()

        return buff

    def _check_block_digest(self):
        # only finalize once the whole block has actually been read,
        # and only ever finalize once (e.g. repeated reads at EOF must
        # not register the same failure again)
        if self.digest_checker.block_status != DigestChecker.STATUS_PENDING:
            return

        check = _compare_digest_rfc_3548(self.block_digester, self.block_digest)
        if check is False:
            self.digest_checker.block_status = DigestChecker.STATUS_FAILED
            self.digest_checker.problem('block digest failed: {}'.format(self.block_digest))
        elif check is True:
            self.digest_checker.block_status = DigestChecker.STATUS_PASSED

    def _check_payload_digest(self):
        if self.digest_checker.payload_status != DigestChecker.STATUS_PENDING:
            return

        check = _compare_digest_rfc_3548(self.payload_digester, self.payload_digest)
        if check is False:
            self.digest_checker.payload_status = DigestChecker.STATUS_FAILED
            self.digest_checker.problem('payload digest failed: {}'.format(self.payload_digest))
        elif check is True:
            self.digest_checker.payload_status = DigestChecker.STATUS_PASSED


def _compare_digest_rfc_3548(digester, digest):
    '''
    The WARC standard does not recommend a digest algorithm and appears to
    allow any encoding from RFC3548. The Python base64 module supports
    RFC3548 although the base64 alternate alphabet is not exactly a first
    class citizen. Hopefully digest algos are named with the same names
    used by OpenSSL.
    '''
    if not digester or not digest:
        return None

    digester_b32 = str(digester)

    our_algo, our_value = _parse_digest(digester_b32)
    warc_algo, warc_value = _parse_digest(digest)

    warc_b32 = _to_b32(len(our_value), warc_value)

    if our_value == warc_b32:
        return True

    return False


def _to_b32(length, value):
    '''
    Convert value to base 32, given that it's supposed to have the same
    length as the digest we're about to compare it to
    '''
    if len(value) == length:
        return value  # casefold needed here? -- rfc recommends not allowing

    if len(value) > length:
        binary = base64.b16decode(value, casefold=True)
    else:
        binary = _b64_wrapper(value)

    return to_native_str(base64.b32encode(binary), encoding='ascii')


base64_url_filename_safe_alt = b'-_'


def _b64_wrapper(value):
    if '-' in value or '_' in value:
        return base64.b64decode(value, altchars=base64_url_filename_safe_alt)
    else:
        return base64.b64decode(value)


def _parse_digest(digest):
    algo, sep, value = digest.partition(':')
    if sep == ':':
        return algo, value
    else:
        raise ValueError('could not parse digest algorithm out of '+digest)
