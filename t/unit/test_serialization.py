#!/usr/bin/python

from __future__ import annotations

from base64 import b64decode
from unittest.mock import Mock, call, patch

import pytest

import t.skip
from kombu.exceptions import ContentDisallowed, DecodeError, EncodeError
from kombu.serialization import (DecoderView, SerializerNotInstalled,
                                 decoder_view, disable_insecure_serializers,
                                 dumps, enable_insecure_serializers, loads,
                                 pickle, pickle_protocol,
                                 prepare_accept_content, raw_encode, register,
                                 register_msgpack, register_pickle,
                                 register_yaml, registry, unregister)
from kombu.utils.encoding import str_to_bytes

# For content_encoding tests
unicode_string = 'abcdé\u8463'
unicode_string_as_utf8 = unicode_string.encode('utf-8')
latin_string = 'abcdé'
latin_string_as_latin1 = latin_string.encode('latin-1')
latin_string_as_utf8 = latin_string.encode('utf-8')


# For serialization tests
py_data = {
    'string': 'The quick brown fox jumps over the lazy dog',
    'int': 10,
    'float': 3.14159265,
    'unicode': 'Thé quick brown fox jumps over thé lazy dog',
    'list': ['george', 'jerry', 'elaine', 'cosmo'],
}

# JSON serialization tests
json_data = """\
{"int": 10, "float": 3.1415926500000002, \
"list": ["george", "jerry", "elaine", "cosmo"], \
"string": "The quick brown fox jumps over the lazy \
dog", "unicode": "Th\\u00e9 quick brown fox jumps over \
th\\u00e9 lazy dog"}\
"""

# Pickle serialization tests
pickle_data = pickle.dumps(py_data, protocol=pickle_protocol)

# YAML serialization tests
yaml_data = """\
float: 3.1415926500000002
int: 10
list: [george, jerry, elaine, cosmo]
string: The quick brown fox jumps over the lazy dog
unicode: "Th\\xE9 quick brown fox jumps over th\\xE9 lazy dog"
"""


msgpack_py_data = dict(py_data)
msgpack_py_data['unicode'] = 'Th quick brown fox jumps over th lazy dog'
# Unicode chars are lost in transmit :(
msgpack_data = b64decode(str_to_bytes("""\
haNpbnQKpWZsb2F0y0AJIftTyNTxpGxpc3SUpmdlb3JnZaVqZXJyeaZlbGFpbmWlY29zbW+mc3Rya\
W5n2gArVGhlIHF1aWNrIGJyb3duIGZveCBqdW1wcyBvdmVyIHRoZSBsYXp5IGRvZ6d1bmljb2Rl2g\
ApVGggcXVpY2sgYnJvd24gZm94IGp1bXBzIG92ZXIgdGggbGF6eSBkb2c=\
"""))


registry.register('testS', lambda s: s, lambda s: 'decoded',
                  'application/testS', 'utf-8')


class test_Serialization:

    def test_disable(self):
        disabled = registry._disabled_content_types
        try:
            registry.disable('testS')
            assert 'application/testS' in disabled
            disabled.clear()

            registry.disable('application/testS')
            assert 'application/testS' in disabled
        finally:
            disabled.clear()

    def test_enable(self):
        registry._disabled_content_types.add('application/json')
        registry.enable('json')
        assert 'application/json' not in registry._disabled_content_types
        registry._disabled_content_types.add('application/json')
        registry.enable('application/json')
        assert 'application/json' not in registry._disabled_content_types

    def test_loads_when_disabled(self):
        disabled = registry._disabled_content_types
        try:
            registry.disable('testS')

            with pytest.raises(SerializerNotInstalled):
                loads('xxd', 'application/testS', 'utf-8', force=False)

            ret = loads('xxd', 'application/testS', 'utf-8', force=True)
            assert ret == 'decoded'
        finally:
            disabled.clear()

    def test_loads_when_data_is_None(self):
        loads(None, 'application/testS', 'utf-8')

    def test_content_type_decoding(self):
        assert loads(
            unicode_string_as_utf8,
            content_type='plain/text',
            content_encoding='utf-8') == unicode_string
        assert loads(
            latin_string_as_latin1,
            content_type='application/data',
            content_encoding='latin-1') == latin_string

    def test_content_type_binary(self):
        assert isinstance(
            loads(unicode_string_as_utf8,
                  content_type='application/data', content_encoding='binary'),
            bytes)

        assert loads(
            unicode_string_as_utf8,
            content_type='application/data',
            content_encoding='binary') == unicode_string_as_utf8

    def test_content_type_encoding(self):
        # Using the 'raw' serializer
        assert (dumps(unicode_string, serializer='raw')[-1] ==
                unicode_string_as_utf8)
        assert (dumps(latin_string, serializer='raw')[-1] ==
                latin_string_as_utf8)
        # And again w/o a specific serializer to check the
        # code where we force unicode objects into a string.
        assert dumps(unicode_string)[-1] == unicode_string_as_utf8
        assert dumps(latin_string)[-1] == latin_string_as_utf8

    def test_enable_insecure_serializers(self):
        with patch('kombu.serialization.registry') as registry:
            enable_insecure_serializers()
            registry.assert_has_calls([
                call.enable('pickle'), call.enable('yaml'),
                call.enable('msgpack'),
            ])
            registry.enable.side_effect = KeyError()
            enable_insecure_serializers()

        with patch('kombu.serialization.registry') as registry:
            enable_insecure_serializers(['msgpack'])
            registry.assert_has_calls([call.enable('msgpack')])

    def test_disable_insecure_serializers(self):
        with patch('kombu.serialization.registry') as registry:
            registry._decoders = ['pickle', 'yaml', 'doomsday']
            disable_insecure_serializers(allowed=['doomsday'])
            registry.disable.assert_has_calls([call('pickle'), call('yaml')])
            registry.enable.assert_has_calls([call('doomsday')])
            disable_insecure_serializers(allowed=None)
            registry.disable.assert_has_calls([
                call('pickle'), call('yaml'), call('doomsday')
            ])

    def test_reraises_EncodeError(self):
        with pytest.raises(EncodeError):
            dumps([object()], serializer='json')

    def test_reraises_DecodeError(self):
        with pytest.raises(DecodeError):
            loads(object(), content_type='application/json',
                  content_encoding='utf-8')

    def test_json_loads(self):
        assert loads(json_data,
                     content_type='application/json',
                     content_encoding='utf-8') == py_data

    def test_json_dumps(self):
        a = loads(
            dumps(py_data, serializer='json')[-1],
            content_type='application/json',
            content_encoding='utf-8',
        )
        b = loads(
            json_data,
            content_type='application/json',
            content_encoding='utf-8',
        )
        assert a == b

    @t.skip.if_pypy
    def test_msgpack_loads(self):
        register_msgpack()
        pytest.importorskip('msgpack')
        res = loads(msgpack_data,
                    content_type='application/x-msgpack',
                    content_encoding='binary')
        assert res == msgpack_py_data

    @t.skip.if_pypy
    def test_msgpack_dumps(self):
        pytest.importorskip('msgpack')
        register_msgpack()
        a = loads(
            dumps(msgpack_py_data, serializer='msgpack')[-1],
            content_type='application/x-msgpack',
            content_encoding='binary',
        )
        b = loads(
            msgpack_data,
            content_type='application/x-msgpack',
            content_encoding='binary',
        )
        assert a == b

    def test_yaml_loads(self):
        pytest.importorskip('yaml')
        register_yaml()
        assert loads(
            yaml_data,
            content_type='application/x-yaml',
            content_encoding='utf-8') == py_data

    @pytest.mark.parametrize('buffer_type', [memoryview, bytearray])
    def test_yaml_loads__buffer(self, buffer_type):
        pytest.importorskip('yaml')
        register_yaml()
        assert loads(
            buffer_type(yaml_data.encode('utf-8')),
            content_type='application/x-yaml',
            content_encoding='utf-8') == py_data

    def test_yaml_loads__utf16_buffer(self):
        pytest.importorskip('yaml')
        register_yaml()
        assert loads(
            memoryview(yaml_data.encode('utf-16')),
            content_type='application/x-yaml',
            content_encoding='utf-16') == py_data

    def test_yaml_dumps(self):
        pytest.importorskip('yaml')
        register_yaml()
        a = loads(
            dumps(py_data, serializer='yaml')[-1],
            content_type='application/x-yaml',
            content_encoding='utf-8',
        )
        b = loads(
            yaml_data,
            content_type='application/x-yaml',
            content_encoding='utf-8',
        )
        assert a == b

    def test_pickle_loads(self):
        assert loads(
            pickle_data,
            content_type='application/x-python-serialize',
            content_encoding='binary') == py_data

    def test_pickle_dumps(self):
        a = pickle.loads(pickle_data),
        b = pickle.loads(dumps(py_data, serializer='pickle')[-1]),
        assert a == b

    def test_register(self):
        register(None, None, None, None)

    def test_unregister(self):
        with pytest.raises(SerializerNotInstalled):
            unregister('nonexisting')
        dumps('foo', serializer='pickle')
        unregister('pickle')
        with pytest.raises(SerializerNotInstalled):
            dumps('foo', serializer='pickle')
        register_pickle()

    def test_set_default_serializer_missing(self):
        with pytest.raises(SerializerNotInstalled):
            registry._set_default_serializer('nonexisting')

    def test_dumps_missing(self):
        with pytest.raises(SerializerNotInstalled):
            dumps('foo', serializer='nonexisting')

    def test_dumps__no_serializer(self):
        ctyp, cenc, data = dumps(str_to_bytes('foo'))
        assert ctyp == 'application/data'
        assert cenc == 'binary'

    def test_loads__trusted_content(self):
        loads('tainted', 'application/data', 'binary', accept=[])
        loads('tainted', 'application/text', 'utf-8', accept=[])

    def test_loads__not_accepted(self):
        with pytest.raises(ContentDisallowed):
            loads('tainted', 'application/x-evil', 'binary', accept=[])
        with pytest.raises(ContentDisallowed):
            loads('tainted', 'application/x-evil', 'binary',
                  accept=['application/x-json'])
        assert loads('tainted', 'application/x-doomsday', 'binary',
                     accept=['application/x-doomsday'])

    def test_raw_encode(self):
        assert raw_encode(b'foo') == (
            'application/data', 'binary', b'foo',
        )

    @pytest.mark.masked_modules('yaml')
    def test_register_yaml__no_yaml(self, mask_modules):
        register_yaml()
        with pytest.raises(SerializerNotInstalled):
            loads('foo', 'application/x-yaml', 'utf-8')

    @pytest.mark.masked_modules('msgpack')
    def test_register_msgpack__no_msgpack(self, mask_modules):
        register_msgpack()
        with pytest.raises(SerializerNotInstalled):
            loads('foo', 'application/x-msgpack', 'utf-8')

    def test_prepare_accept_content(self):
        assert {'application/json'} == prepare_accept_content(['json'])
        assert {'application/json'} == prepare_accept_content(
            ['application/json'])

    def test_prepare_accept_content_bad_serializer(self):
        with pytest.raises(SerializerNotInstalled):
            prepare_accept_content(['bad_serializer'])


class test_DecoderView:
    """Frozen decoder capabilities must be isolated per consumer."""

    content_type = 'application/x-frozen'
    other_type = 'application/x-later'

    def setup_method(self):
        self.registry_snapshot = (
            dict(registry._decoders),
            set(registry._disabled_content_types),
            dict(registry.name_to_type),
            dict(registry.type_to_name),
        )
        self.decoder = Mock(name='decoder', return_value='frozen')
        register('frozen', None, self.decoder, self.content_type)
        self.view = decoder_view()

    def teardown_method(self):
        decoders, disabled, name_to_type, type_to_name = (
            self.registry_snapshot)
        registry._decoders.clear()
        registry._decoders.update(decoders)
        registry._disabled_content_types.clear()
        registry._disabled_content_types.update(disabled)
        registry.name_to_type.clear()
        registry.name_to_type.update(name_to_type)
        registry.type_to_name.clear()
        registry.type_to_name.update(type_to_name)
        registry._set_default_serializer('json')

    def test_view_is_frozen_snapshot(self):
        assert self.view.content_types == frozenset(registry._decoders)
        assert self.view.decoder_for(self.content_type) is self.decoder
        assert self.view.decoders[self.content_type] is self.decoder
        assert self.view.aliases()['frozen'] == self.content_type

    def test_decoders_registered_later_are_not_mixed_in(self):
        later = Mock(name='later', return_value='later')
        register('later', None, later, self.other_type)

        assert self.other_type not in self.view.content_types
        assert self.view.decoder_for(self.other_type) is None
        with pytest.raises(ContentDisallowed):
            self.view.decode(b'payload', self.other_type, 'binary')
        later.assert_not_called()

    def test_global_disable_does_not_affect_view(self):
        registry.disable('frozen')
        self.decoder.reset_mock()

        assert self.view.decode(b'payload', self.content_type, 'binary') \
            == 'frozen'
        self.decoder.assert_called_once_with(b'payload')

    def test_global_enable_does_not_affect_view(self):
        registry.disable('frozen')
        view = decoder_view()

        registry.enable('frozen')
        with pytest.raises(ContentDisallowed):
            view.decode(b'payload', self.content_type, 'binary')

    def test_refuses_before_running_decoder(self):
        registry.disable('frozen')
        view = decoder_view()
        self.decoder.reset_mock()

        with pytest.raises(ContentDisallowed):
            view.decode(b'payload', self.content_type, 'binary')
        self.decoder.assert_not_called()

    def test_accept_alias_cannot_be_bypassed_by_header_alias(self):
        view = decoder_view(accept=['json'])

        # The message header uses the serializer name instead of the
        # content type: it resolves to the same content type and must be
        # refused.
        with pytest.raises(ContentDisallowed):
            view.decode(b'payload', 'frozen', 'binary')
        self.decoder.assert_not_called()

    def test_accept_allows_aliases_in_whitelist(self):
        view = decoder_view(accept=['frozen'])

        assert view.accept == {self.content_type}
        assert view.decode(b'payload', 'frozen', 'binary') == 'frozen'
        assert view.decode(
            b'payload', self.content_type, 'binary') == 'frozen'

    def test_accept_allows_raw_content_types(self):
        view = decoder_view(accept=[self.content_type])

        assert view.accept == {self.content_type}
        assert view.decode(b'payload', self.content_type, 'binary') == \
            'frozen'

    def test_accept_bad_serializer(self):
        with pytest.raises(SerializerNotInstalled):
            decoder_view(accept=['missing-serializer'])

    def test_trusted_content_is_never_refused(self):
        view = decoder_view(accept=[])

        assert view.decode(b'payload', 'application/data', 'binary') == \
            b'payload'
        assert view.decode(b'payload', 'application/text', 'utf-8') == \
            'payload'
        # ``text/plain`` is not trusted content, same as ``loads``.
        with pytest.raises(ContentDisallowed):
            view.decode(b'payload', 'text/plain', 'utf-8')

    def test_with_accept_keeps_frozen_capabilities(self):
        view = self.view.with_accept([self.content_type])
        register('later', None, Mock(), self.other_type)

        assert view.accept == {self.content_type}
        assert view.content_types == self.view.content_types
        assert view.decoder_for(self.other_type) is None

    def test_with_accept_none_restores_default_behaviour(self):
        view = self.view.with_accept(None)

        assert view.accept is None
        assert view.decode(b'payload', self.content_type, 'binary') == \
            'frozen'

    def test_decoder_view_accept_from_content_type(self):
        view = decoder_view(accept=['application/json'])
        assert view.accept == {'application/json'}

    def test_unknown_content_type_without_decoder_is_passed_through(self):
        assert self.view.decode(b'payload', 'text/plain', 'utf-8') == 'payload'
        assert self.view.decode(b'payload', 'binary/thing', 'binary') == \
            b'payload'

    def test_repr(self):
        assert 'frozen' in repr(self.view)

    def test_explicit_disabled_is_recorded(self):
        view = DecoderView(
            {self.content_type: self.decoder},
            disabled=[self.content_type],
        )

        assert view.accept is None
        with pytest.raises(ContentDisallowed):
            view.decode(b'payload', self.content_type, 'binary')
        self.decoder.assert_not_called()

    def test_decode_wraps_decoder_errors(self):
        decoder = Mock(name='decoder', side_effect=ValueError('boom'))
        view = DecoderView({self.content_type: decoder})

        with pytest.raises(DecodeError):
            view.decode(b'payload', self.content_type, 'binary')

    def test_decode_unwraps_content_type_bytes(self):
        assert self.view.decode(
            b'payload', str_to_bytes(self.content_type), 'binary',
        ) == 'frozen'
