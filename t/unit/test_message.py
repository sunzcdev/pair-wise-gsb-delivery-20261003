from __future__ import annotations

import sys
from unittest.mock import Mock, patch

import pytest

from kombu.exceptions import ContentDisallowed
from kombu.message import Message
from kombu.serialization import DecoderView


class test_Message:

    def test_repr(self):
        assert repr(Message('b', channel=Mock()))

    def test_decode(self):
        m = Message('body', channel=Mock())
        decode = m._decode = Mock()
        assert m._decoded_cache is None
        assert m.decode() is m._decode.return_value
        assert m._decoded_cache is m._decode.return_value
        m._decode.assert_called_with()
        m._decode = Mock()
        assert m.decode() is decode.return_value

    def test_reraise_error(self):
        m = Message('body', channel=Mock())
        callback = Mock(name='callback')
        try:
            raise KeyError('foo')
        except KeyError:
            m.errors.append(sys.exc_info())
        m._reraise_error(callback)
        callback.assert_called()

        with pytest.raises(KeyError):
            m._reraise_error(None)

    @patch('kombu.message.decompress')
    def test_decompression_stores_error(self, decompress):
        decompress.side_effect = RuntimeError()
        m = Message('body', channel=Mock(), headers={'compression': 'zlib'})
        with pytest.raises(RuntimeError):
            m._reraise_error(None)

    def test_decoder_view_defaults_to_none(self):
        assert Message('body', channel=Mock()).decoder_view is None

    def test_decode_uses_decoder_view(self):
        view = Mock(name='decoder_view')
        m = Message(
            b'body', channel=Mock(),
            content_type='application/json', content_encoding='utf-8',
            decoder_view=view,
        )

        m.decode()

        view.decode.assert_called_with(
            b'body', 'application/json', 'utf-8',
        )

    def test_decode_without_view_uses_registry(self):
        with patch('kombu.message.loads') as loads:
            m = Message(
                b'body', channel=Mock(),
                content_type='application/json', content_encoding='utf-8',
                accept={'application/json'},
            )
            m.decode()

        loads.assert_called_with(
            b'body', 'application/json', 'utf-8',
            accept={'application/json'},
        )

    def test_decoder_view_refusal_prevents_decoding(self):
        decoder = Mock(name='decoder')
        view = DecoderView(
            {'application/x-python-serialize': decoder},
            disabled=['application/x-python-serialize'],
        )
        m = Message(
            b'body', channel=Mock(),
            content_type='application/x-python-serialize',
            content_encoding='binary', decoder_view=view,
        )

        with pytest.raises(ContentDisallowed):
            m.decode()
        decoder.assert_not_called()

    def test_decoder_view_rejects_unregistered_decoder(self):
        decoder = Mock(name='decoder', return_value='decoded')
        view = DecoderView({})
        m = Message(
            b'body', channel=Mock(),
            content_type='application/x-frozen', content_encoding='binary',
            decoder_view=view,
        )

        # No decoder was frozen in the view, so the raw payload is returned
        # and no decoder from the global registry is ever used.
        assert m.decode() == b'body'
        decoder.assert_not_called()
