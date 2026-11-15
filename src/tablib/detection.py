"""
    tablib.detection
    ~~~~~~~~~~~~~~~~

    Format auto-detection with candidate reporting.

    A single piece of text can be valid in several formats at once (a single
    row CSV may also be valid JSON, JSON is also valid YAML, ...), so probing
    reports *every* plausible format together with the evidence that was
    gathered, a confidence category, and whether the outcome is ambiguous.

    Detection never consumes a borrowed stream: seekable streams have their
    position restored, while non-seekable streams are sampled once and the
    sample is replayed in front of the remaining data (bounded replay), so
    the real import loses nothing.  Detection never installs or imports
    optional format dependencies either -- only already registered formats
    are probed, and binary signatures are classified with ``zipfile``-free
    header inspection.
"""

__lazy_modules__ = {"io"}

from dataclasses import dataclass, field
from io import BytesIO, StringIO

from .formats import registry
from .utils import normalize_input

#: Maximum number of bytes/chars read from a non-seekable stream to detect
#: its format.  The sample is replayed in front of the remaining data for
#: the real import, so nothing is lost, but replay is bounded to this size.
DEFAULT_SAMPLE_SIZE = 64 * 1024

# Confidence categories.
SIGNATURE = 'signature'   # a binary/file signature (magic bytes) matched
STRUCTURE = 'structure'   # the content was successfully parsed as the format
HEURISTIC = 'heuristic'   # weak textual sniffing (e.g. csv.Sniffer)

_CONFIDENCE_RANK = {SIGNATURE: 3, STRUCTURE: 2, HEURISTIC: 1}

# Formats whose detect() relies on delimiter sniffing rather than a real parse.
_HEURISTIC_FORMATS = frozenset({'csv', 'tsv'})

_OLE2_MAGIC = b'\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1'
_ZIP_MAGIC = b'PK\x03\x04'
_GZIP_MAGIC = b'\x1f\x8b'
_KNOWN_MAGICS = (_OLE2_MAGIC, _ZIP_MAGIC, _GZIP_MAGIC)


@dataclass
class FormatCandidate:
    """Evidence that a stream may be in a given format."""

    key: str         #: registry key of the format, e.g. ``'csv'``
    title: str       #: title of the format
    confidence: str  #: one of ``signature``, ``structure`` or ``heuristic``
    evidence: str    #: human-readable description of why the format matched


@dataclass
class DetectionResult:
    """Outcome of :func:`detect_format_candidates`."""

    #: Plausible formats, most confident first.
    candidates: list = field(default_factory=list)
    #: Reasons why the input (partially) could not be identified.
    unknown_reasons: list = field(default_factory=list)
    #: A stream positioned at the start of the content, safe to import from.
    stream: object = None

    @property
    def best(self):
        """The most confident candidate, or ``None`` if the format is unknown."""
        return self.candidates[0] if self.candidates else None

    @property
    def is_ambiguous(self):
        """True when more than one format plausibly matches the input."""
        return len(self.candidates) > 1

    @property
    def is_unknown(self):
        """True when no format matched the input."""
        return not self.candidates

    def explain(self):
        """Return a human-readable summary of the detection outcome."""
        lines = [
            f'{candidate.title}: {candidate.confidence} -- {candidate.evidence}'
            for candidate in self.candidates
        ]
        lines += [f'unknown: {reason}' for reason in self.unknown_reasons]
        return '\n'.join(lines) or 'no evidence gathered'


class ReplayStream:
    """Prepend an already-consumed sample back onto a non-seekable stream.

    Detection must never eat the head of a borrowed stream; when the source
    cannot be seeked, the sampled prefix is replayed from here before the
    remaining data is read through, so the real import sees the complete
    content.  Only the bounded sample is ever buffered.
    """

    def __init__(self, prefix, stream):
        self._prefix = prefix
        self._pos = 0
        self._stream = stream
        self._empty = b'' if isinstance(prefix, bytes) else ''
        self._newline = b'\n' if isinstance(prefix, bytes) else '\n'

    def read(self, size=-1):
        if self._pos >= len(self._prefix):
            return self._stream.read(size)
        if size is None or size < 0:
            chunk = self._prefix[self._pos:]
            self._pos = len(self._prefix)
            rest = self._stream.read()
            return chunk + rest if rest else chunk
        chunk = self._prefix[self._pos:self._pos + size]
        self._pos += len(chunk)
        if len(chunk) < size:
            rest = self._stream.read(size - len(chunk))
            if rest:
                chunk = chunk + rest
        return chunk

    def readline(self, size=-1):
        limit = None if size is None or size < 0 else size
        if self._pos < len(self._prefix):
            remaining = self._prefix[self._pos:] if limit is None \
                else self._prefix[self._pos:self._pos + limit]
            index = remaining.find(self._newline)
            if index != -1:
                line = remaining[:index + 1]
                self._pos += len(line)
                return line
            self._pos += len(remaining)
            if limit is not None and len(remaining) >= limit:
                return remaining
            tail = self._readline_from_source(
                None if limit is None else limit - len(remaining))
            return remaining + tail
        return self._readline_from_source(limit)

    def _readline_from_source(self, limit):
        readline = getattr(self._stream, 'readline', None)
        if readline is not None:
            return readline(-1 if limit is None else limit)
        # Rare fallback for streams without readline(): read one unit at a time.
        parts = []
        while limit is None or limit > 0:
            chunk = self._stream.read(1)
            if not chunk:
                break
            parts.append(chunk)
            if chunk == self._newline:
                break
            if limit is not None:
                limit -= 1
        return self._empty.join(parts)

    def __iter__(self):
        return self

    def __next__(self):
        line = self.readline()
        if line == self._empty:
            raise StopIteration
        return line

    def seekable(self):
        return False

    def readable(self):
        return True


def _seek_position(stream):
    """Return the current position of `stream`, or None if it is not seekable."""
    try:
        if hasattr(stream, 'seekable') and not stream.seekable():
            return None
        return stream.tell()
    except Exception:
        return None


def _classify_zip(sample):
    """Best-effort classification of a ZIP container from its first bytes.

    Returns ``'xlsx'``, ``'ods'`` or ``None``.  Only local file headers
    present in the sample are inspected, so no optional dependency is needed.
    """
    if len(sample) < 30:
        return None
    name_length = int.from_bytes(sample[26:28], 'little')
    name = sample[30:30 + name_length]
    if len(name) < name_length:
        return None
    if name == b'mimetype':
        # OpenDocument files store an uncompressed 'mimetype' entry first.
        return 'ods'
    # An OOXML package always contains a '[Content_Types].xml' part; entry
    # ordering varies between producers, so scan the sampled local headers.
    if b'[Content_Types].xml' in sample:
        return 'xlsx'
    return None


def _is_truncated_signature(sample):
    """True if `sample` is a strict prefix of a known binary signature."""
    return any(
        len(sample) < len(magic) and magic.startswith(sample)
        for magic in _KNOWN_MAGICS
    )


def _looks_binary(sample):
    if b'\x00' in sample:
        return True
    try:
        sample.decode('utf-8')
    except UnicodeDecodeError:
        return True
    return False


def detect_format_candidates(stream, formats=None, sample_size=DEFAULT_SAMPLE_SIZE):
    """Probe `stream` and return a :class:`DetectionResult`.

    Unlike :func:`tablib.detect_format`, which silently returns the first
    format that matches, this reports every plausible format together with
    its evidence and confidence category, and flags ambiguous or unknown
    input.

    The stream is never consumed: seekable streams have their position
    restored, and for non-seekable streams the sampled prefix (at most
    `sample_size` bytes/chars) is replayed in front of the remaining data.
    ``result.stream`` is positioned at the start of the content and is safe
    to import from.  Optional format dependencies are never installed or
    imported for detection; only already registered formats are probed.

    :param formats: optional iterable of registry keys restricting the probing.
    :param sample_size: maximum amount of data read from a non-seekable stream.
    """
    stream = normalize_input(stream)
    unknown_reasons = []

    if not hasattr(stream, 'read'):
        # Not a stream at all (e.g. a pandas DataFrame): hand the object to
        # the formats' detect() methods directly.
        position = None
        sample = None
        out_stream = stream

        def make_view():
            return stream
    else:
        position = _seek_position(stream)
        if position is None:
            # Non-seekable input: read a bounded sample once and replay it in
            # front of the remaining data so the real import loses nothing.
            sample = stream.read(sample_size)
            out_stream = ReplayStream(sample, stream)

            def make_view():
                return BytesIO(sample) if isinstance(sample, bytes) else StringIO(sample)
        else:
            sample = stream.read(sample_size)
            stream.seek(position)
            out_stream = stream

            def make_view():
                stream.seek(position)
                return stream

    # Binary signature gate: incomplete signatures or unsupported compression
    # are reported as unknown without running any real detection.
    hints = {}
    skip_probing = False
    if isinstance(sample, bytes) and sample:
        if _is_truncated_signature(sample):
            unknown_reasons.append(
                'truncated file signature: the input starts like a known '
                'binary format but is incomplete'
            )
            skip_probing = True
        elif sample.startswith(_GZIP_MAGIC):
            unknown_reasons.append(
                'gzip-compressed data: decompress it before importing'
            )
            skip_probing = True
        elif sample.startswith(_ZIP_MAGIC):
            kind = _classify_zip(sample)
            if kind == 'xlsx':
                hints['xlsx'] = 'ZIP container with an OOXML "[Content_Types].xml" part'
            elif kind == 'ods':
                hints['ods'] = 'OpenDocument ZIP container ("mimetype" entry first)'
            else:
                unknown_reasons.append('ZIP container with an unrecognized layout')
        elif sample.startswith(_OLE2_MAGIC):
            hints['xls'] = 'OLE2 compound document signature'

    candidates = []
    probed_keys = set()
    if not skip_probing:
        for key, fmt in registry.items():
            if formats is not None and key not in formats:
                continue
            probed_keys.add(key)
            try:
                matched = bool(fmt.detect(make_view()))
            except AttributeError:
                matched = False  # the format has no detect()
            except Exception:
                matched = False
            finally:
                if position is not None:
                    try:
                        stream.seek(position)
                    except Exception:
                        pass
            hint = hints.get(key)
            if matched:
                if hint:
                    confidence, evidence = SIGNATURE, hint
                elif key in _HEURISTIC_FORMATS:
                    confidence, evidence = HEURISTIC, 'delimiter sniffing matched'
                else:
                    confidence = STRUCTURE
                    evidence = f'content parses as {fmt.title}'
                candidates.append(FormatCandidate(key, fmt.title, confidence, evidence))
            elif hint:
                candidates.append(FormatCandidate(
                    key, fmt.title, SIGNATURE,
                    hint + '; the content could not be fully parsed',
                ))

    for key, evidence in hints.items():
        if key not in probed_keys and (formats is None or key in formats):
            unknown_reasons.append(
                f'the input looks like {key} ({evidence}) but the {key} format '
                'is not registered (optional dependency not installed?)'
            )

    if (
        not candidates and not unknown_reasons
        and isinstance(sample, bytes) and sample and _looks_binary(sample)
    ):
        unknown_reasons.append('unrecognized binary signature')

    # Stable sort: most confident first, registration order breaks ties.
    candidates.sort(key=lambda candidate: -_CONFIDENCE_RANK[candidate.confidence])
    return DetectionResult(candidates, unknown_reasons, out_stream)
