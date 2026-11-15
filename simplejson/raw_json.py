"""Implementation of RawJSON and ValidatedRawJSON
"""
from .compat import binary_type, text_type
from .decoder import JSONDecoder

__all__ = ['RawJSON', 'ValidatedRawJSON']


class RawJSON(object):
    """Wrap an encoded JSON document for direct embedding in the output

    The wrapped text is trusted as-is and emitted verbatim; it is not
    validated. Use :class:`ValidatedRawJSON` to check the fragment once
    at construction time instead.

    """
    def __init__(self, encoded_json):
        self.encoded_json = encoded_json


# Shared strict decoder used to validate fragments exactly once, at
# construction time. The defaults (strict=True, allow_nan=False) reject
# non-standard numbers such as NaN, Infinity and -Infinity, and
# decode() rejects any trailing data after the first complete value.
_strict_decoder = JSONDecoder()


class ValidatedRawJSON(RawJSON):
    """Wrap an encoded JSON fragment that is validated at construction

    The fragment must contain exactly one complete JSON value, with
    optional leading and trailing whitespace. Truncated documents,
    a trailing second value, and non-standard numbers (``NaN``,
    ``Infinity``, ``-Infinity``) are rejected by raising
    :class:`JSONDecodeError` from the constructor.

    The validated fragment is stored as immutable text, so its content
    cannot be changed by external objects after validation. When the
    fragment is embedded in an object or array, the original text is
    emitted verbatim (preserving its byte meaning) and no re-parsing
    takes place during encoding.

    """
    def __init__(self, encoded_json):
        if isinstance(encoded_json, (binary_type, bytearray)):
            encoded_json = bytes(encoded_json).decode('utf-8')
        if not isinstance(encoded_json, text_type):
            raise TypeError(
                'ValidatedRawJSON fragment must be text or UTF-8 bytes, '
                'got %s' % (type(encoded_json).__name__,))
        # Normalize to an exact, immutable text object: a text subclass
        # instance is copied to a plain str so the stored fragment
        # cannot be mutated after it has been validated.
        encoded_json = text_type(encoded_json)
        _strict_decoder.decode(encoded_json)
        self.encoded_json = encoded_json
