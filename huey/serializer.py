import gzip
import hashlib
import hmac
import logging
import pickle
import struct
import zlib

from huey.exceptions import ConfigurationError
from huey.exceptions import SerializerError
from huey.utils import encode


logger = logging.getLogger('huey.serializer')


# ---------------------------------------------------------------------------
# Versioned-envelope constants.
#
# A versioned message is laid out as::
#
#     magic (4 bytes) | version (1 byte) | flags (1 byte) |
#     original length (8 bytes, big-endian unsigned) |
#     stored length (8 bytes, big-endian unsigned) |
#     checksum (32 bytes, sha256 of the *uncompressed* payload) |
#     encoded payload
#
# The fixed header is therefore 54 bytes. Every piece of information needed to
# pick a decoding path (serialization format and compression codec) is stored
# explicitly inside the envelope instead of being guessed from the first byte.
# ---------------------------------------------------------------------------
ENVELOPE_MAGIC = b'HUEY'
ENVELOPE_VERSION = 1
ENVELOPE_HEADER_SIZE = 54
_ENVELOPE_HEADER = struct.Struct('>4sBBQQ32s')

# Flags byte: high nibble stores the serialization format, low nibble stores
# the compression codec.
FORMAT_PICKLE = 0x10

COMPRESSION_NONE = 0x00
COMPRESSION_GZIP = 0x01
COMPRESSION_ZLIB = 0x02

# Codec tables used both when writing and when selecting a decode path.
_COMPRESSORS = {
    COMPRESSION_GZIP: lambda d, level: gzip.compress(d, level),
    COMPRESSION_ZLIB: lambda d, level: zlib.compress(d, level),
}
_DECOMPRESSORS = {
    COMPRESSION_GZIP: gzip.decompress,
    COMPRESSION_ZLIB: zlib.decompress,
}


def is_compressed(data):
    """Legacy heuristic used to detect un-enveloped (old-style) messages.

    Only retained for backwards-compatible decoding of messages that were
    written before the versioned envelope existed. New code must rely on the
    compression codec recorded in the envelope instead.
    """
    return data and (data[0] == 0x1f or data[0] == 0x78)


class Serializer(object):
    def __init__(self, compression=False, compression_level=6, use_zlib=False,
                 pickle_protocol=pickle.HIGHEST_PROTOCOL,
                 allow_legacy=True):
        self.comp = compression
        self.comp_level = compression_level
        self.use_zlib = use_zlib
        self.pickle_protocol = pickle_protocol or pickle.HIGHEST_PROTOCOL
        # When True (the default) messages written by older Huey releases that
        # do not carry a versioned envelope can still be read. This only
        # applies to messages that fail the explicit envelope magic check, so
        # new messages can never be mistaken for legacy ones.
        self.allow_legacy = allow_legacy

    # -- object <-> bytes (pickle by default) -------------------------------
    def _serialize(self, data):
        return pickle.dumps(data, self.pickle_protocol)

    def _deserialize(self, data):
        return pickle.loads(data)

    # -- compression ---------------------------------------------------------
    def _compression_codec(self):
        if not self.comp:
            return COMPRESSION_NONE
        return COMPRESSION_ZLIB if self.use_zlib else COMPRESSION_GZIP

    def _compress(self, data, codec):
        if codec == COMPRESSION_NONE:
            return data
        try:
            compressor = _COMPRESSORS[codec]
        except KeyError:
            raise SerializerError('Unknown compression codec 0x%02x recorded '
                                  'in envelope.' % codec)
        return compressor(data, self.comp_level)

    def _decompress(self, data, codec):
        if codec == COMPRESSION_NONE:
            return data
        try:
            decompressor = _DECOMPRESSORS[codec]
        except KeyError:
            raise SerializerError('Unknown compression codec 0x%02x recorded '
                                  'in envelope.' % codec)
        return decompressor(data)

    # -- envelope handling ---------------------------------------------------
    def _build_envelope(self, payload):
        """Wrap the serialized *payload* in a versioned envelope."""
        codec = self._compression_codec()
        flags = FORMAT_PICKLE | codec
        original_length = len(payload)
        checksum = hashlib.sha256(payload).digest()
        encoded = self._compress(payload, codec)
        header = _ENVELOPE_HEADER.pack(
            ENVELOPE_MAGIC,
            ENVELOPE_VERSION,
            flags,
            original_length,
            len(encoded),
            checksum)
        return header + encoded

    def _is_envelope(self, data):
        # Match the full magic (and not just the first byte) so that a legacy
        # message whose first byte happens to equal the magic's can never be
        # mis-identified as an envelope.
        return data[:len(ENVELOPE_MAGIC)] == ENVELOPE_MAGIC

    def _parse_envelope(self, data):
        # Truncated header: reject before anything is deserialized.
        if len(data) < ENVELOPE_HEADER_SIZE:
            raise SerializerError('Message is truncated: expected at least %d '
                                  'envelope header bytes, got %d.' %
                                  (ENVELOPE_HEADER_SIZE, len(data)))

        magic, version, flags, original_length, stored_length, checksum = (
            _ENVELOPE_HEADER.unpack_from(data, 0))

        if magic != ENVELOPE_MAGIC:
            raise SerializerError('Invalid envelope magic %r.' % magic)
        if version != ENVELOPE_VERSION:
            raise SerializerError('Unknown envelope version %d (supported '
                                  'version is %d).' %
                                  (version, ENVELOPE_VERSION))

        format_code = flags & 0xf0
        codec = flags & 0x0f
        if format_code != FORMAT_PICKLE:
            raise SerializerError('Unknown serialization format 0x%02x '
                                  'recorded in envelope.' % format_code)
        if codec not in _DECOMPRESSORS and codec != COMPRESSION_NONE:
            raise SerializerError('Unknown compression codec 0x%02x recorded '
                                  'in envelope.' % codec)

        payload = data[ENVELOPE_HEADER_SIZE:]
        # Truncated / length-mismatched body.
        if len(payload) != stored_length:
            raise SerializerError(
                'Envelope length mismatch: header declares %d stored bytes '
                'but message contains %d.' %
                (stored_length, len(payload)))

        if codec != COMPRESSION_NONE:
            try:
                payload = self._decompress(payload, codec)
            except (OSError, EOFError, zlib.error) as exc:
                raise SerializerError(
                    'Failed to decompress message payload: %s' % exc)

        if len(payload) != original_length:
            raise SerializerError(
                'Envelope length mismatch: header declares %d original bytes '
                'but decoded payload contains %d.' %
                (original_length, len(payload)))

        if hashlib.sha256(payload).digest() != checksum:
            raise SerializerError(
                'Envelope checksum mismatch: the message is corrupted or has '
                'been tampered with.')

        return format_code, codec, payload

    # -- public API ----------------------------------------------------------
    def serialize(self, data):
        payload = self._serialize(data)
        return self._build_envelope(payload)

    def deserialize(self, data):
        if not data:
            raise SerializerError('No data to deserialize.')
        if self._is_envelope(data):
            format_code, codec, payload = self._parse_envelope(data)
            return self._deserialize(payload)
        if self.allow_legacy:
            return self._deserialize_legacy(data)
        raise SerializerError(
            'Message is not a valid versioned envelope and legacy message '
            'compatibility is disabled.')

    def _deserialize_legacy(self, data):
        """Decode an old-style, un-enveloped message."""
        if self.comp:
            if not is_compressed(data):
                logger.warning('compression enabled but message data does not '
                               'appear to be compressed.')
            elif self.use_zlib:
                data = zlib.decompress(data)
            else:
                data = gzip.decompress(data)
        return self._deserialize(data)


def constant_time_compare(s1, s2):
    return hmac.compare_digest(s1, s2)


class SignedSerializer(Serializer):
    def __init__(self, secret=None, salt='huey', **kwargs):
        super(SignedSerializer, self).__init__(**kwargs)
        if not secret or not salt:
            raise ConfigurationError('The secret and salt parameters are '
                                     'required by %r' % type(self))
        self.secret = encode(secret)
        self.salt = encode(salt)
        self.separator = b':'
        self._key = hashlib.sha1(self.salt + self.secret).digest()

    def _signature(self, message):
        signature = hmac.new(self._key, msg=message, digestmod=hashlib.sha1)
        return signature.hexdigest().encode('utf8')

    def _sign(self, message):
        return message + self.separator + self._signature(message)

    def _unsign(self, signed):
        if self.separator not in signed:
            raise ValueError('Separator "%s" not found' % self.separator)

        msg, sig = signed.rsplit(self.separator, 1)
        if constant_time_compare(sig, self._signature(msg)):
            return msg

        raise ValueError('Signature "%s" mismatch!' % sig)

    def serialize(self, data):
        # Sign the complete envelope so the magic, version, format flags,
        # lengths and checksum are all covered by the signature - the format
        # fields can never be altered without invalidating it.
        payload = self._serialize(data)
        envelope = self._build_envelope(payload)
        return self._sign(envelope)

    def deserialize(self, data):
        if not data:
            raise SerializerError('No data to deserialize.')
        # The envelope magic sits at the start of the signed bytes, so the
        # signature layer does not interfere with envelope detection.
        if self._is_envelope(data):
            envelope = self._unsign(data)
            format_code, codec, payload = self._parse_envelope(envelope)
            return self._deserialize(payload)
        if self.allow_legacy:
            # Old-style messages were signed before compression/enveloping.
            return self._deserialize(self._unsign(data))
        raise SerializerError(
            'Message is not a valid versioned envelope and legacy message '
            'compatibility is disabled.')
