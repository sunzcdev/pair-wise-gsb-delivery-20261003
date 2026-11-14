import pytest

from io import BytesIO

from warcio.archiveiterator import ArchiveIterator
from warcio.recordbuilder import RecordBuilder
from warcio.recordloader import ArcWarcRecord
from warcio.revisit import (
    RevisitResolver,
    RevisitContent,
    RevisitPayload,
    RevisitReference,
    resolve_revisit,
    IDENTICAL_PAYLOAD_DIGEST_PROFILES,
)
from warcio.statusandheaders import StatusAndHeaders
from warcio.exceptions import (
    RevisitResolveError,
    RevisitReferenceError,
    RevisitCycleError,
    RevisitChainLengthError,
    RevisitProfileError,
    RevisitTargetTypeError,
    RevisitDigestError,
    RevisitPayloadError,
)
from warcio.warcwriter import BufferWARCWriter

from . import get_test_file


BODY = b'<html><body>hello revisit</body></html>\n'
BODY_2 = b'<html><body>a different body</body></html>\n'

V1_0_PROFILE = 'http://netpreserve.org/warc/1.0/revisit/identical-payload-digest'
V1_1_PROFILE = 'http://netpreserve.org/warc/1.1/revisit/identical-payload-digest'


# ============================================================================
def make_response(uri='http://example.com/page', payload=BODY,
                  date='2020-01-01T00:00:00Z', content_type='text/html',
                  extra_http=(), warc_headers=None):
    builder = RecordBuilder()
    http_headers = StatusAndHeaders('200 OK',
                                    [('Content-Type', content_type),
                                     ('Content-Length', str(len(payload)))]
                                    + list(extra_http),
                                    protocol='HTTP/1.0')
    return builder.create_warc_record(uri, 'response',
                                      payload=BytesIO(payload),
                                      length=len(payload),
                                      http_headers=http_headers,
                                      warc_headers_dict=dict(
                                          {'WARC-Date': date},
                                          **(warc_headers or {})))


def make_revisit(uri='http://example.com/page', payload_digest=None,
                 refers_to=None, refers_to_uri=None, refers_to_date=None,
                 date='2020-01-02T00:00:00Z', profile=V1_0_PROFILE,
                 http_headers=None, extra_headers=None, body=b''):
    builder = RecordBuilder()
    headers = {'WARC-Date': date}

    if profile is not None:
        headers['WARC-Profile'] = profile

    if refers_to is not None:
        headers['WARC-Refers-To'] = refers_to
    if refers_to_uri is not None:
        headers['WARC-Refers-To-Target-URI'] = refers_to_uri
    if refers_to_date is not None:
        headers['WARC-Refers-To-Date'] = refers_to_date
    if payload_digest is not None:
        headers['WARC-Payload-Digest'] = payload_digest

    headers.update(extra_headers or {})

    if http_headers is None:
        http_headers = StatusAndHeaders('200 OK',
                                        [('Content-Type', 'text/html')],
                                        protocol='HTTP/1.0')

    return builder.create_warc_record(uri, 'revisit',
                                      payload=BytesIO(body) if body else None,
                                      length=len(body),
                                      http_headers=http_headers,
                                      warc_headers_dict=headers)


def record_id(record):
    return record.rec_headers.get_header('WARC-Record-ID')


def payload_digest(record):
    return record.rec_headers.get_header('WARC-Payload-Digest')


def digests_match(record, other_record):
    return payload_digest(record) == payload_digest(other_record)


def lookup_from(*records):
    by_id = {}
    by_uri_date = {}
    for record in records:
        by_id[record_id(record)] = record
        key = (record.rec_headers.get_header('WARC-Target-URI'),
               record.rec_headers.get_header('WARC-Date'))
        by_uri_date.setdefault(key, record)

    def lookup(reference):
        if reference.kind == RevisitReference.RECORD_ID:
            return by_id.get(reference.record_id)
        return by_uri_date.get((reference.uri, reference.date))

    return lookup


# ============================================================================
class TestResolveBasic(object):
    def test_resolve_by_record_id(self):
        response = make_response()
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response))
        # the revisit must refer to the response record id, so rebuild it
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response))

        content = RevisitResolver(lookup_from(response, revisit)).resolve(revisit)

        assert content.read_payload() == BODY
        assert content.payload_record is response
        assert content.revisit is revisit
        assert content.digest_verified is True
        assert content.length == len(BODY)

    def test_resolve_by_uri_and_date(self):
        response = make_response(date='2020-01-01T00:00:00Z')
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to_uri='http://example.com/page',
                               refers_to_date='2020-01-01T00:00:00Z')

        content = RevisitResolver(lookup_from(response)).resolve(revisit)
        assert content.read_payload() == BODY

    def test_resolve_using_separate_callbacks(self):
        response = make_response()
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response))

        resolver = RevisitResolver(by_record_id=lambda rid: (
            response if rid == record_id(response) else None))
        assert resolver.resolve(revisit).read_payload() == BODY

    def test_resolve_using_mapping(self):
        response = make_response()
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response))

        content = RevisitResolver({record_id(response): response}).resolve(revisit)
        assert content.read_payload() == BODY

    def test_resolve_convenience_function(self):
        response = make_response()
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response))

        content = resolve_revisit(revisit, lookup_from(response))
        assert content.read_payload() == BODY
        assert content.payload.length == len(BODY)

    def test_resolve_via_record_method(self):
        response = make_response()
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response))

        content = revisit.resolve_revisit(lookup_from(response))
        assert content.read_payload() == BODY

    def test_profile_fallback_to_own_uri_and_date(self):
        response = make_response(uri='http://example.com/page',
                                 date='2020-01-01T00:00:00Z')
        revisit = make_revisit(uri='http://example.com/page',
                               date='2020-01-01T00:00:00Z',
                               payload_digest=payload_digest(response),
                               profile=V1_1_PROFILE)

        content = RevisitResolver(lookup_from(response)).resolve(revisit)
        assert content.read_payload() == BODY
        assert content.payload_record is response


# ============================================================================
class TestMetadataProvenance(object):
    def test_http_metadata_comes_from_revisit(self):
        response = make_response(
            payload=BODY,
            extra_http=[('Content-Type', 'text/html; charset=utf-8')])
        revisit_headers = StatusAndHeaders(
            '203 Non-Authoritative Information',
            [('Content-Type', 'text/html; charset=utf-8'),
             ('X-Revisit', 'yes'),
             ('Content-Length', str(len(BODY)))],
            protocol='HTTP/1.1')
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response),
                               http_headers=revisit_headers)

        content = RevisitResolver(lookup_from(response)).resolve(revisit)

        assert content.http_headers is revisit_headers
        assert content.statusline == '203 Non-Authoritative Information'
        assert content.http_headers.get_header('X-Revisit') == 'yes'
        assert content.payload_record.http_headers is response.http_headers
        assert content.rec_headers.get_header('WARC-Type') == 'revisit'
        # the payload record is not substituted for the revisit
        assert content.payload_record is not revisit
        assert content.payload_record.rec_headers.get_header('WARC-Type') == 'response'

    def test_warc_headers_kept_from_revisit(self):
        response = make_response()
        revisit = make_revisit(uri='http://example.com/other',
                               date='2020-06-06T00:00:00Z',
                               payload_digest=payload_digest(response),
                               refers_to=record_id(response),
                               extra_headers={'WARC-Warcinfo-ID': '<urn:uuid:1>'})

        content = RevisitResolver(lookup_from(response)).resolve(revisit)

        assert content.target_uri == 'http://example.com/other'
        assert content.rec_headers.get_header('WARC-Date') == '2020-06-06T00:00:00Z'
        assert content.rec_headers.get_header('WARC-Warcinfo-ID') == '<urn:uuid:1>'
        assert content.read_payload() == BODY

    def test_revisit_http_headers_not_mutated(self):
        response = make_response()
        revisit_headers = StatusAndHeaders('200 OK',
                                           [('Content-Type', 'text/html')],
                                           protocol='HTTP/1.0')
        revisit = make_revisit(payload_digest=payload_digest(response),
                               refers_to=record_id(response),
                               http_headers=revisit_headers)

        content = RevisitResolver(lookup_from(response)).resolve(revisit)
        content.read_content()

        assert revisit_headers.headers == [('Content-Type', 'text/html')]
        assert revisit.http_headers is revisit_headers


# ============================================================================
class TestChains(object):
    def test_chain_of_two(self):
        response = make_response()
        middle = make_revisit(payload_digest=payload_digest(response),
                              refers_to=record_id(response))
        outer = make_revisit(payload_digest=payload_digest(response),
                             refers_to=record_id(middle),
                             date='2020-03-03T00:00:00Z')

        content = RevisitResolver(lookup_from(response, middle)).resolve(outer)

        assert content.read_payload() == BODY
        assert content.payload_record is response
        assert content.chain == (outer, middle)
        assert len(content.chain) == 2

    def test_chain_respects_limit(self):
        response = make_response()
        chain = [response]
        for _ in range(4):
            chain.append(make_revisit(payload_digest=payload_digest(response),
                                      refers_to=record_id(chain[-1])))

        resolver = RevisitResolver(lookup_from(*chain[:-1]), max_chain_length=3)
        with pytest.raises(RevisitChainLengthError):
            resolver.resolve(chain[-1])

        # exactly at the limit is fine
        resolver = RevisitResolver(lookup_from(*chain[:-1]), max_chain_length=4)
        assert resolver.resolve(chain[-1]).read_payload() == BODY

    def test_chain_limit_must_be_positive(self):
        with pytest.raises(ValueError):
            RevisitResolver(lookup_from(), max_chain_length=0)

    def test_cycle_detected(self):
        a = make_revisit(refers_to='<urn:uuid:b>', payload_digest='sha1:AAAA',
                         extra_headers={'WARC-Record-ID': '<urn:uuid:a>'})
        b = make_revisit(refers_to='<urn:uuid:a>', payload_digest='sha1:AAAA',
                         date='2020-02-02T00:00:00Z',
                         extra_headers={'WARC-Record-ID': '<urn:uuid:b>'})

        with pytest.raises(RevisitCycleError) as excinfo:
            RevisitResolver({record_id(a): a, record_id(b): b}).resolve(a)

        assert len(excinfo.value.chain) >= 2

    def test_self_reference_is_cycle(self):
        revisit = make_revisit(refers_to='<urn:uuid:self>', payload_digest='sha1:AAAA')
        by_id = {'<urn:uuid:self>': revisit}

        with pytest.raises(RevisitCycleError):
            RevisitResolver(by_id).resolve(revisit)

    def test_repeating_uri_date_chain_hits_limit(self):
        # without record ids a self-referencing URI/date chain can only be
        # stopped by the configured chain limit
        revisit = make_revisit(refers_to_uri='http://example.com/page',
                               refers_to_date='2020-01-02T00:00:00Z',
                               payload_digest='sha1:AAAA')

        def lookup(reference):
            # always hand back an equivalent revisit without a record id
            return make_revisit(refers_to_uri='http://example.com/page',
                                refers_to_date='2020-01-02T00:00:00Z',
                                payload_digest='sha1:AAAA',
                                extra_headers={'WARC-Record-ID': None})

        with pytest.raises(RevisitChainLengthError):
            RevisitResolver(lookup, max_chain_length=3).resolve(revisit)

    def test_missing_reference(self):
        revisit = make_revisit(refers_to='<urn:uuid:missing>',
                               payload_digest='sha1:AAAA')

        with pytest.raises(RevisitReferenceError) as excinfo:
            RevisitResolver({}).resolve(revisit)

        assert excinfo.value.reference.record_id == '<urn:uuid:missing>'
        assert excinfo.value.record is revisit

    def test_no_reference_at_all(self):
        revisit = make_revisit(payload_digest='sha1:AAAA', profile=None)

        with pytest.raises(RevisitReferenceError):
            RevisitResolver({}).resolve(revisit)

    def test_missing_reference_in_middle_of_chain(self):
        middle = make_revisit(refers_to='<urn:uuid:missing>',
                              payload_digest='sha1:AAAA')
        outer = make_revisit(refers_to=record_id(middle),
                             payload_digest='sha1:AAAA',
                             date='2020-04-04T00:00:00Z')

        with pytest.raises(RevisitReferenceError) as excinfo:
            RevisitResolver(lookup_from(middle)).resolve(outer)

        assert excinfo.value.record is middle


# ============================================================================
class TestTargetTypes(object):
    def test_request_target_rejected(self):
        builder = RecordBuilder()
        request = builder.create_warc_record(
            'http://example.com/page', 'request',
            payload=BytesIO(b'GET / HTTP/1.0\r\n\r\n'),
            length=18,
            http_headers=StatusAndHeaders('GET / HTTP/1.0', [],
                                          protocol='HTTP/1.0',
                                          is_http_request=True))
        revisit = make_revisit(refers_to=record_id(request),
                               payload_digest='sha1:AAAA')

        with pytest.raises(RevisitTargetTypeError) as excinfo:
            RevisitResolver(lookup_from(request)).resolve(revisit)

        assert excinfo.value.target_record is request

    def test_warcinfo_target_rejected(self):
        builder = RecordBuilder()
        warcinfo = builder.create_warcinfo_record('test.warc', {'software': 'x'})
        revisit = make_revisit(refers_to=record_id(warcinfo),
                               payload_digest='sha1:AAAA')

        with pytest.raises(RevisitTargetTypeError):
            RevisitResolver(lookup_from(warcinfo)).resolve(revisit)

    def test_resource_target_accepted(self):
        builder = RecordBuilder()
        resource = builder.create_warc_record('http://example.com/page',
                                              'resource',
                                              payload=BytesIO(BODY),
                                              length=len(BODY))
        revisit = make_revisit(refers_to=record_id(resource),
                               payload_digest=payload_digest(resource))

        content = RevisitResolver(lookup_from(resource)).resolve(revisit)
        assert content.read_payload() == BODY

    def test_custom_target_types(self):
        builder = RecordBuilder()
        metadata = builder.create_warc_record('http://example.com/page',
                                              'metadata',
                                              payload=BytesIO(BODY),
                                              length=len(BODY))
        revisit = make_revisit(refers_to=record_id(metadata),
                               payload_digest=payload_digest(metadata))

        resolver = RevisitResolver(lookup_from(metadata),
                                   payload_record_types=('response', 'metadata'))
        assert resolver.resolve(revisit).read_payload() == BODY

    def test_not_a_revisit(self):
        response = make_response()
        with pytest.raises(RevisitResolveError):
            RevisitResolver(lookup_from(response)).resolve(response)


# ============================================================================
class TestDigestVerification(object):
    def test_digest_mismatch_raises(self):
        response = make_response()
        revisit = make_revisit(
            refers_to=record_id(response),
            payload_digest='sha1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA')

        with pytest.raises(RevisitDigestError) as excinfo:
            RevisitResolver(lookup_from(response)).resolve(revisit)

        assert excinfo.value.expected == 'sha1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
        assert excinfo.value.actual == payload_digest(response)

    def test_digest_mismatch_returns_no_content(self):
        response = make_response()
        revisit = make_revisit(
            refers_to=record_id(response),
            payload_digest='sha1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA')

        with pytest.raises(RevisitDigestError):
            RevisitResolver(lookup_from(response)).resolve(revisit)

    def test_mismatch_in_chain_raises(self):
        response = make_response()
        middle = make_revisit(payload_digest=payload_digest(response),
                              refers_to=record_id(response))
        outer = make_revisit(
            refers_to=record_id(middle),
            payload_digest='sha1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
            date='2020-05-05T00:00:00Z')

        with pytest.raises(RevisitDigestError):
            RevisitResolver(lookup_from(response, middle)).resolve(outer)

    def test_no_digest_allowed_by_default(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response))

        content = RevisitResolver(lookup_from(response)).resolve(revisit)
        assert content.read_payload() == BODY
        assert content.digest_verified is False

    def test_require_digest(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response))

        resolver = RevisitResolver(lookup_from(response), require_digest=True)
        with pytest.raises(RevisitDigestError):
            resolver.resolve(revisit)

    def test_verify_target_digest_detects_wrong_record(self):
        # a different response with the same URI/date as the reference
        wrong = make_response(payload=BODY_2)
        revisit = make_revisit(refers_to=record_id(wrong),
                               payload_digest='sha1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA')

        resolver = RevisitResolver(lookup_from(wrong), verify_target_digest=True)
        with pytest.raises(RevisitDigestError):
            resolver.resolve(revisit)

    def test_hex_digest_accepted(self):
        import hashlib
        response = make_response()
        hex_digest = 'sha1:' + hashlib.sha1(BODY).hexdigest()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=hex_digest)

        content = RevisitResolver(lookup_from(response)).resolve(revisit)
        assert content.read_payload() == BODY

    def test_unknown_algorithm_raises(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest='notahash:AAAA')

        with pytest.raises(RevisitDigestError):
            RevisitResolver(lookup_from(response)).resolve(revisit)

    def test_malformed_digest_raises(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest='nocolon')

        with pytest.raises(RevisitDigestError):
            RevisitResolver(lookup_from(response)).resolve(revisit)


# ============================================================================
class TestProfiles(object):
    def test_created_revisit_record_resolves(self):
        # RecordBuilder.create_revisit_record() produces the same shape the
        # resolver expects: WARC-Refers-To-Target-URI + Date + digest
        builder = RecordBuilder()
        response = make_response(uri='http://example.com/page',
                                 date='2020-01-01T00:00:00Z')
        revisit = builder.create_revisit_record(
            'http://example.com/page',
            digest=payload_digest(response),
            refers_to_uri='http://example.com/page',
            refers_to_date='2020-01-01T00:00:00Z')

        assert revisit.rec_headers.get_header('WARC-Profile') == V1_0_PROFILE

        content = RevisitResolver(lookup_from(response)).resolve(revisit)
        assert content.read_payload() == BODY
        assert content.rec_headers.get_header('WARC-Type') == 'revisit'

    def test_unsupported_profile_rejected(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response),
                               profile='http://example.com/other-profile')

        with pytest.raises(RevisitProfileError):
            RevisitResolver(lookup_from(response)).resolve(revisit)

    def test_unsupported_profile_allowed_when_not_strict(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response),
                               profile='http://example.com/other-profile')

        resolver = RevisitResolver(lookup_from(response), strict_profiles=False)
        assert resolver.resolve(revisit).read_payload() == BODY

    def test_server_not_available_profile_rejected(self):
        response = make_response()
        revisit = make_revisit(
            refers_to=record_id(response),
            payload_digest=payload_digest(response),
            profile='http://netpreserve.org/warc/1.0/revisit/server-not-available')

        with pytest.raises(RevisitProfileError):
            RevisitResolver(lookup_from(response)).resolve(revisit)

    def test_no_profile_is_accepted(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response),
                               profile=None)

        assert RevisitResolver(lookup_from(response)).resolve(revisit).read_payload() == BODY


# ============================================================================
class TestStreamOwnership(object):
    def test_bytes_stream_not_closed(self):
        class TrackingStream(BytesIO):
            closed_by_resolver = False

            def close(self):
                self.closed_by_resolver = True
                super(TrackingStream, self).close()

        stream = TrackingStream(BODY)
        builder = RecordBuilder()
        response = builder.create_warc_record(
            'http://example.com/page', 'response',
            payload=stream, length=len(BODY),
            http_headers=StatusAndHeaders('200 OK', [], protocol='HTTP/1.0'))
        stream.seek(0)

        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))

        RevisitResolver(lookup_from(response)).resolve(revisit).read_payload()

        assert stream.closed_by_resolver is False
        assert stream.closed is False
        # the caller is free to close it afterwards
        stream.close()

    def test_repeated_resolution_is_independent(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))

        # a callback that returns the same record object each time; the
        # response's own raw_stream is consumed by the first resolution
        response.raw_stream.seek(0)
        resolver = RevisitResolver(lookup_from(response))

        first = resolver.resolve(revisit)
        assert first.read_payload() == BODY

        second = resolver.resolve(revisit)
        assert second.read_payload() == BODY

    def test_repeated_resolution_without_cache(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))

        resolver = RevisitResolver(lookup_from(response), cache_payloads=False)

        first = resolver.resolve(revisit)
        assert first.read_payload() == BODY
        # read a second time from the same content, then discard
        assert first.read_payload() == BODY
        first.close()

        # fresh record for the second resolution
        response_2 = make_response()
        revisit_2 = make_revisit(refers_to=record_id(response_2),
                                 payload_digest=payload_digest(response_2))
        resolver_2 = RevisitResolver(lookup_from(response_2), cache_payloads=False)
        second = resolver_2.resolve(revisit_2)
        assert second.read_payload() == BODY

    def test_reader_cursors_are_independent(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))

        content = RevisitResolver(lookup_from(response)).resolve(revisit)

        reader_a = content.open_payload()
        reader_b = content.open_payload()

        chunk = reader_a.read(5)
        assert chunk == BODY[:5]
        assert reader_b.read(5) == BODY[:5]
        assert reader_a.read(5) == BODY[5:10]

        reader_a.close()
        # closing one reader does not affect another
        assert reader_b.read() == BODY[5:]

    def test_content_close_releases_uncached_payload(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))

        resolver = RevisitResolver(lookup_from(response), cache_payloads=False)
        content = resolver.resolve(revisit)
        assert content.read_payload() == BODY

        content.close()
        assert content.closed is True
        with pytest.raises(ValueError):
            content.open_payload()

    def test_cached_payload_survives_content_close(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))

        with RevisitResolver(lookup_from(response)) as resolver:
            content = resolver.resolve(revisit)
            content.close()

            # cache still holds the payload
            again = resolver.resolve(revisit)
            assert again.read_payload() == BODY

        with pytest.raises(ValueError):
            again.open_payload()

    def test_stream_read_failure_propagates(self):
        class BrokenStream(object):
            def read(self, size=-1):
                raise IOError('boom')

        response = make_response()
        response.raw_stream = BrokenStream()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))

        with pytest.raises(IOError):
            RevisitResolver(lookup_from(response)).resolve(revisit)

    def test_cache_reuses_payload_across_record_objects(self):
        # a lookup may hand out a fresh record object for the same record id
        original = make_response()
        original_id = record_id(original)
        digest = payload_digest(original)

        reads = []

        class CountingStream(BytesIO):
            def read(self, size=-1):
                reads.append(size)
                return super(CountingStream, self).read(size)

        def lookup(reference):
            builder = RecordBuilder()
            clone = builder.create_warc_record(
                'http://example.com/page', 'response',
                payload=BytesIO(BODY), length=len(BODY),
                http_headers=StatusAndHeaders('200 OK', [],
                                              protocol='HTTP/1.0'))
            clone.rec_headers.replace_header('WARC-Record-ID', original_id)
            clone.raw_stream = CountingStream(BODY)
            return clone

        def revisit_at(date):
            return make_revisit(refers_to=original_id, payload_digest=digest,
                                date=date)

        resolver = RevisitResolver(lookup)

        first = resolver.resolve(revisit_at('2020-07-01T00:00:00Z'))
        assert first.read_payload() == BODY
        reads_after_first = len(reads)
        assert reads_after_first > 0

        # the payload is cached under the shared WARC-Record-ID, so the clone
        # returned the second time is never read from
        second = resolver.resolve(revisit_at('2020-07-02T00:00:00Z'))
        assert second.read_payload() == BODY
        assert len(reads) == reads_after_first

    def test_cache_is_bounded(self):
        records = [make_response() for _ in range(4)]
        builder_lookup = {}
        for record in records:
            builder_lookup[record_id(record)] = record

        resolver = RevisitResolver(builder_lookup, max_cached_payloads=2)

        contents = []
        for record in records:
            revisit = make_revisit(refers_to=record_id(record),
                                   payload_digest=payload_digest(record))
            contents.append(resolver.resolve(revisit))

        assert len(resolver._payload_cache) == 2
        # the two most recent payloads are still readable, while the payload
        # buffers of the evicted ones were closed
        assert contents[-1].read_payload() == BODY
        with pytest.raises(ValueError):
            contents[0].read_payload()

    def test_cache_flag_invalid(self):
        with pytest.raises(ValueError):
            RevisitResolver({}, max_cached_payloads=0)

    def test_consumed_stream_with_digest_raises(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))
        # the callback's stream was already read elsewhere
        response.raw_stream.read()

        with pytest.raises(RevisitResolveError):
            RevisitResolver(lookup_from(response)).resolve(revisit)

    def test_consumed_stream_without_digest_raises(self):
        response = make_response()
        revisit = make_revisit(refers_to=record_id(response))
        response.raw_stream.read()

        with pytest.raises(RevisitPayloadError):
            RevisitResolver(lookup_from(response)).resolve(revisit)


# ============================================================================
class TestDecodedContent(object):
    def test_chunked_payload_decoded(self):
        import hashlib
        chunked = b'5\r\nhello\r\n0\r\n\r\n'
        builder = RecordBuilder()
        response = builder.create_warc_record(
            'http://example.com/page', 'response',
            payload=BytesIO(chunked), length=len(chunked),
            http_headers=StatusAndHeaders(
                '200 OK',
                [('Content-Type', 'text/plain'),
                 ('Transfer-Encoding', 'chunked')],
                protocol='HTTP/1.0'))
        revisit = make_revisit(
            refers_to=record_id(response),
            payload_digest='sha1:' + base32(hashlib.sha1(chunked).digest()))

        content = RevisitResolver(lookup_from(response)).resolve(revisit)

        # payload access returns the bytes as stored
        assert content.read_payload() == chunked
        # decoded access applies the original record's transfer encoding
        assert content.read_content() == b'hello'

    def test_decoded_content_repeatable(self):
        import gzip
        compressed = gzip.compress(BODY)
        builder = RecordBuilder()
        response = builder.create_warc_record(
            'http://example.com/page', 'response',
            payload=BytesIO(compressed), length=len(compressed),
            http_headers=StatusAndHeaders(
                '200 OK',
                [('Content-Type', 'text/html'),
                 ('Content-Encoding', 'gzip')],
                protocol='HTTP/1.0'))
        revisit = make_revisit(
            refers_to=record_id(response),
            payload_digest=payload_digest(response))

        content = RevisitResolver(lookup_from(response)).resolve(revisit)

        assert content.read_content() == BODY
        assert content.read_content() == BODY
        assert b''.join(content.iter_content(4)) == BODY


# ============================================================================
class TestFromArchive(object):
    def test_resolve_records_read_from_warc(self):
        writer = BufferWARCWriter(gzip=False)
        writer.write_record(make_response())

        stream = writer.get_stream()
        stream.seek(0)

        resolved = []
        for response in ArchiveIterator(stream):
            assert response.rec_type == 'response'

            revisit = make_revisit(refers_to=record_id(response),
                                   payload_digest=payload_digest(response))

            # resolve while the response record is still unread; the lookup
            # returns a record that is currently being streamed
            content = RevisitResolver(lookup_from(response)).resolve(revisit)
            resolved.append(content.read_payload())

        assert resolved == [BODY]

    def test_resolve_revisit_from_stream(self):
        writer = BufferWARCWriter(gzip=False)
        response = make_response()
        writer.write_record(response)

        revisit = make_revisit(refers_to=record_id(response),
                               payload_digest=payload_digest(response))
        writer.write_record(revisit)

        stream = writer.get_stream()
        stream.seek(0)

        # collect the payload of the response into a re-readable copy first,
        # as WARC streams are single pass
        response_copy = None
        seen_revisit = None
        for record in ArchiveIterator(stream):
            if record.rec_type == 'response':
                raw = record.raw_stream.read()
                response_copy = RevisitPayload()
                response_copy._write(raw)
                response_copy._store_digest('sha1', _digester_for(raw))
            elif record.rec_type == 'revisit':
                seen_revisit = record

        assert seen_revisit is not None
        assert seen_revisit.rec_headers.get_header('WARC-Profile') == V1_0_PROFILE

        # the lookup may return any record-like object for the reference; here
        # it is backed by the re-readable payload copy
        stand_in = _RecordProxy('response', response_copy.open())
        content = RevisitResolver(lambda reference: stand_in).resolve(seen_revisit)

        assert content.read_payload() == BODY
        # the revisit's own HTTP metadata is still what is exposed
        assert content.http_headers is seen_revisit.http_headers
        response_copy.close()


class _RecordProxy(object):
    """ Minimal record-like object for the tests. """

    def __init__(self, rec_type, raw_stream, rec_headers=None,
                 http_headers=None):
        self.rec_type = rec_type
        self.raw_stream = raw_stream
        self.rec_headers = rec_headers or StatusAndHeaders('', [], protocol='')
        self.http_headers = http_headers
        self.content_type = None
        self.length = None

    def content_stream(self):
        from warcio.recordloader import content_stream_for
        return content_stream_for(self.http_headers, self.raw_stream)


# ============================================================================
class TestRealArchiveData(object):
    def test_chunked_warc_record(self):
        # the response in this file stores a chunked + gzip payload; its
        # WARC-Payload-Digest covers the payload exactly as stored
        with open(get_test_file('example-iana.org-chunked.warc'), 'rb') as fh:
            for record in ArchiveIterator(fh):
                if record.rec_type != 'response':
                    continue

                stored = record.raw_stream.read()
                # this capture uses the alternate hex encoding of the digest
                stored_digest = 'sha1:' + __import__('hashlib').sha1(
                    stored).hexdigest()

                assert record.rec_headers.get_header(
                    'WARC-Payload-Digest') == stored_digest

                payload = RevisitPayload()
                payload._write(stored)
                target = _RecordProxy('response', payload.open(),
                                      http_headers=record.http_headers)

                revisit = make_revisit(
                    uri=record.rec_headers.get_header('WARC-Target-URI'),
                    refers_to='<urn:uuid:whatever>',
                    payload_digest=record.rec_headers.get_header(
                        'WARC-Payload-Digest'),
                    http_headers=record.http_headers)

                content = RevisitResolver(lambda ref: target).resolve(revisit)

                assert content.read_payload() == stored
                # the decoded body differs from the stored bytes
                decoded = content.read_content()
                assert decoded != stored
                assert b'<html' in decoded.lower()
                break
            else:  # pragma: no cover - test data always has a response
                pytest.fail('no response record in test data')

    def test_wrong_record_in_real_archive_rejected(self):
        with open(get_test_file('example-iana.org-chunked.warc'), 'rb') as fh:
            record = next(r for r in ArchiveIterator(fh)
                          if r.rec_type == 'response')
            stored = record.raw_stream.read()

        payload = RevisitPayload()
        payload._write(stored)
        target = _RecordProxy('response', payload.open())

        revisit = make_revisit(
            refers_to='<urn:uuid:whatever>',
            payload_digest='sha1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA')

        with pytest.raises(RevisitDigestError):
            RevisitResolver(lambda ref: target).resolve(revisit)


def _digester_for(data):
    from warcio.utils import Digester
    digester = Digester('sha1')
    digester.update(data)
    return digester



# ============================================================================
def base32(data):
    import base64
    from warcio.utils import to_native_str
    return to_native_str(base64.b32encode(data))
