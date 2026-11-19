import pickle
import string

import pytest

from fsspec.caching import (
    AdaptiveReadAheadCache,
    BackgroundBlockCache,
    BlockCache,
    BytesCache,
    FirstChunkCache,
    MMapCache,
    ReadAheadCache,
    caches,
    register_cache,
)
from fsspec.implementations.cached import WholeFileCacheFileSystem
from fsspec.implementations.memory import MemoryFileSystem
from fsspec.spec import AbstractBufferedFile


def test_cache_getitem(Cache_imp):
    cacher = Cache_imp(4, letters_fetcher, len(string.ascii_letters))
    assert cacher._fetch(0, 4) == b"abcd"
    assert cacher._fetch(None, 4) == b"abcd"
    assert cacher._fetch(2, 4) == b"cd"
    assert cacher._fetch(0, None) == string.ascii_letters.encode()


def test_block_cache_lru():
    # BlockCache is a cache that stores blocks of data and uses LRU to evict
    block_size = 4
    cache = BlockCache(
        block_size, letters_fetcher, len(string.ascii_letters), maxblocks=2
    )
    # miss
    cache._fetch(0, 2)
    assert cache.cache_info().misses == 1
    assert cache.cache_info().currsize == 1
    assert cache.total_requested_bytes == block_size * cache.miss_count
    assert cache.size == 52

    # hit
    cache._fetch(0, 2)
    assert cache.cache_info().misses == 1
    assert cache.cache_info().currsize == 1
    assert cache.total_requested_bytes == block_size * cache.miss_count

    # hit
    cache._fetch(0, 2)
    assert cache.cache_info().misses == 1
    assert cache.cache_info().currsize == 1
    # this works as a counter since all the reads are from the cache
    assert cache.hit_count == 3
    assert cache.miss_count == 1
    # so far only 4 bytes have been read using range requests
    assert cache.total_requested_bytes == block_size * cache.miss_count

    # miss
    cache._fetch(4, 6)
    assert cache.cache_info().misses == 2
    assert cache.cache_info().currsize == 2
    assert cache.total_requested_bytes == block_size * cache.miss_count

    # miss & evict
    cache._fetch(12, 13)
    assert cache.cache_info().misses == 3
    assert cache.cache_info().currsize == 2
    assert cache.hit_count == 5
    assert cache.miss_count == 3
    assert cache.total_requested_bytes == block_size * cache.miss_count


@pytest.mark.parametrize("cache_cls", [BlockCache, BackgroundBlockCache])
@pytest.mark.parametrize("size", [1, 6, 8])
def test_block_cache_final_range(cache_cls, size):
    data = b"abcdefgh"[:size]
    requests = []

    def fetch(start, end):
        assert 0 <= start < end <= len(data)
        requests.append((start, end))
        return data[start:end]

    cache = cache_cls(4, fetch, len(data))
    try:
        assert cache._fetch(0, None) == data
        expected = [(start, min(start + 4, size)) for start in range(0, size, 4)]
        assert requests == expected
        assert cache.total_requested_bytes == size
        assert cache._fetch(size - 1, size) == data[-1:]
        assert requests == expected
    finally:
        if isinstance(cache, BackgroundBlockCache):
            cache.close()


def test_block_cache_lru_no_redundant_reads():
    block_size = 4
    maxblocks = 2
    cache = BlockCache(
        block_size, letters_fetcher, len(string.ascii_letters), maxblocks=maxblocks
    )
    cache._fetch(0, block_size * (maxblocks + 1))
    assert cache.cache_info().misses == 3


def test_background_block_cache_no_redundant_reads():
    block_size = 4
    maxblocks = 2
    cache = BackgroundBlockCache(
        block_size, letters_fetcher, len(string.ascii_letters), maxblocks=maxblocks
    )
    try:
        # More blocks than the LRU holds: each block is fetched once, not again
        # after being evicted while the earlier ones were prefetched.
        data = cache._fetch(0, block_size * (maxblocks + 1))
        assert data == string.ascii_letters[: block_size * (maxblocks + 1)].encode()
        assert cache.cache_info().misses == maxblocks + 1
    finally:
        cache.close()


def test_background_block_cache_read_ending_on_block_boundary():
    block_size = 4
    cache = BackgroundBlockCache(block_size, letters_fetcher, len(string.ascii_letters))
    try:
        # Only block 0 is needed; block 1 is left to the background prefetch.
        assert cache._fetch(0, block_size) == b"abcd"
        assert cache.cache_info().misses == 1
        assert cache._fetch(block_size, 2 * block_size) == b"efgh"
        assert cache._fetch(2, 2 * block_size) == b"cdefgh"
    finally:
        cache.close()


def test_first_cache():
    """
    FirstChunkCache is a cache that only caches the first chunk of data
    when some of that first block is requested.
    """
    block_size = 5
    cache = FirstChunkCache(block_size, letters_fetcher, len(string.ascii_letters))
    assert cache.cache is None
    assert cache._fetch(12, 15) == letters_fetcher(12, 15)
    assert cache.miss_count == 1
    assert cache.hit_count == 0
    assert cache.cache is None
    total_requested_bytes = 15 - 12
    assert cache.total_requested_bytes == total_requested_bytes

    # because we overlap with the cache range, it will be cached
    assert cache._fetch(3, 10) == letters_fetcher(3, 10)
    assert cache.miss_count == 2
    assert cache.hit_count == 0
    # we'll read the first 5 and then the rest
    total_requested_bytes += block_size + 5
    assert cache.total_requested_bytes == total_requested_bytes

    # partial hit again
    assert cache._fetch(3, 10) == letters_fetcher(3, 10)
    assert cache.miss_count == 2
    assert cache.hit_count == 1
    # we have the first 5 bytes cached
    total_requested_bytes += 10 - 5
    assert cache.total_requested_bytes == total_requested_bytes

    assert cache.cache == letters_fetcher(0, 5)
    assert cache._fetch(0, 4) == letters_fetcher(0, 4)
    assert cache.hit_count == 2
    assert cache.miss_count == 2
    assert cache.total_requested_bytes == 18


def test_readahead_cache():
    """
    ReadAheadCache is a cache that reads ahead of the requested range.
    If the access pattern is not sequential it will be very inefficient.
    """
    block_size = 5
    cache = ReadAheadCache(block_size, letters_fetcher, len(string.ascii_letters))
    assert cache._fetch(12, 15) == letters_fetcher(12, 15)
    assert cache.miss_count == 1
    assert cache.hit_count == 0
    total_requested_bytes = 15 - 12 + block_size
    assert cache.total_requested_bytes == total_requested_bytes

    assert cache._fetch(3, 10) == letters_fetcher(3, 10)
    assert cache.miss_count == 2
    assert cache.hit_count == 0
    assert len(cache.cache) == 12
    total_requested_bytes += (10 - 3) + block_size
    assert cache.total_requested_bytes == total_requested_bytes

    # caache hit again
    assert cache._fetch(3, 10) == letters_fetcher(3, 10)
    assert cache.miss_count == 2
    assert cache.hit_count == 1
    assert len(cache.cache) == 12
    assert cache.total_requested_bytes == total_requested_bytes
    assert cache.cache == letters_fetcher(3, 15)

    # cache miss
    assert cache._fetch(0, 4) == letters_fetcher(0, 4)
    assert cache.hit_count == 1
    assert cache.miss_count == 3
    assert len(cache.cache) == 9
    total_requested_bytes += (4 - 0) + block_size
    assert cache.total_requested_bytes == total_requested_bytes


def _tracking_fetcher(data):
    """Return a fetcher recording every requested (start, end) interval."""
    calls = []

    def fetch(start, end):
        calls.append((start, end))
        assert 0 <= start < end <= len(data)
        return data[start:end]

    return fetch, calls


def test_adaptive_readahead_registered_and_pickleable():
    assert caches["adaptive_readahead"] is AdaptiveReadAheadCache
    cache = AdaptiveReadAheadCache(8, letters_fetcher, len(string.ascii_letters))
    assert cache.adaptive is True
    cache._fetch(0, 4)
    restored = pickle.loads(pickle.dumps(cache))
    assert isinstance(restored, AdaptiveReadAheadCache)
    assert restored.adaptive is True
    assert restored._fetch(0, 6) == letters_fetcher(0, 6)


def test_readahead_adaptive_keyword_equivalent():
    data = string.ascii_letters.encode()
    fetch, calls = _tracking_fetcher(data)
    cache = ReadAheadCache(8, fetch, len(data), adaptive=True)
    assert cache.name == "readahead"
    assert cache.adaptive is True
    # before any sequential pattern is established, only demand is fetched
    assert cache._fetch(10, 12) == data[10:12]
    assert calls == [(10, 12)]


def test_adaptive_readahead_random_trace_does_not_prefetch():
    """Random small reads pull exactly the demanded bytes, never blocks."""
    data = string.ascii_letters.encode()
    fetch, calls = _tracking_fetcher(data)
    cache = AdaptiveReadAheadCache(8, fetch, len(data))
    trace = [40, 0, 20, 44, 4, 26, 10, 34, 16, 48]

    for start in trace:
        assert cache._fetch(start, start + 2) == data[start : start + 2]

    assert calls == [(start, start + 2) for start in trace]
    assert cache.prefetch_bytes == 0
    assert cache.wasted_prefetch_bytes == 0
    assert cache.total_requested_bytes == 2 * len(trace)
    assert cache.demand_bytes == 2 * len(trace)


def test_adaptive_readahead_sequential_trace_ramps_then_reads_ahead():
    """Forward contiguous reads progressively enable the block read-ahead."""
    data = string.ascii_letters.encode()
    fetch, calls = _tracking_fetcher(data)
    cache = AdaptiveReadAheadCache(8, fetch, len(data))

    for start in range(0, 24, 2):
        assert cache._fetch(start, start + 2) == data[start : start + 2]

    # demand-only, then half a block, then full blocksize read-ahead
    assert calls[0] == (0, 2)
    assert calls[1] == (2, 6)
    assert calls[2] == (6, 16)
    assert all(end - start == 10 for start, end in calls[2:])
    assert cache.prefetch_bytes > 0
    assert cache.demand_bytes + cache.prefetch_bytes == cache.total_requested_bytes


def test_adaptive_readahead_reverse_seek_is_bounded():
    """A reverse seek replaces the window with a bounded, demand-only fetch."""
    data = string.ascii_letters.encode()
    fetch, calls = _tracking_fetcher(data)
    cache = AdaptiveReadAheadCache(8, fetch, len(data))

    assert cache._fetch(40, 44) == data[40:44]
    assert cache._fetch(0, 4) == data[0:4]

    assert calls == [(40, 44), (0, 4)]
    # no request is allowed to speculate all the way to the end of the file
    assert all(end - start <= 8 for start, end in calls)
    assert cache._forward_streak == 0


def test_adaptive_readahead_interleaved_windows_stop_prefetching():
    """Repeated window jumps burn the waste budget and disable prefetch."""
    data = string.ascii_letters.encode()
    fetch, calls = _tracking_fetcher(data)
    cache = AdaptiveReadAheadCache(
        8,
        fetch,
        len(data),
        min_prefetch_bytes=2,
        max_wasted_prefetch_bytes=4,
    )

    # two forward steps build a small prefetch window, then we jump away
    for base in (0, 20, 40):
        cache._fetch(base, base + 2)
        cache._fetch(base + 2, base + 4)
        cache._fetch(base + 10, base + 12)

    assert cache.wasted_prefetch_bytes >= 4
    assert cache._prefetch_disabled is True

    before = len(calls)
    assert cache._fetch(48, 50) == data[48:50]
    # once disabled, fetches carry no speculative bytes
    assert calls[before] == (48, 50)
    # every single fetch stays well below a whole-file read
    assert all(end - start <= 10 for start, end in calls)


def test_adaptive_readahead_single_fetch_capped_by_waste_budget():
    data = string.ascii_letters.encode()
    fetch, calls = _tracking_fetcher(data)
    cache = AdaptiveReadAheadCache(64, fetch, len(data), max_wasted_prefetch_bytes=4)
    cache._fetch(0, 2)
    cache._fetch(2, 4)
    cache._fetch(4, 6)
    # even in a sequential run, one fetch never exceeds demand + budget
    assert all(end - start <= 2 + 4 for start, end in calls)


def test_adaptive_readahead_zero_waste_budget_disables_prefetch():
    data = string.ascii_letters.encode()
    fetch, calls = _tracking_fetcher(data)
    cache = AdaptiveReadAheadCache(8, fetch, len(data), max_wasted_prefetch_bytes=0)
    cache._fetch(0, 2)
    cache._fetch(2, 4)
    cache._fetch(4, 6)
    assert calls == [(0, 2), (2, 4), (4, 6)]
    assert cache.prefetch_bytes == 0
    assert cache.total_requested_bytes == 6


def test_adaptive_readahead_short_read_retries_once_then_clamps():
    """A source short read gets one bounded retry, then the window is clamped."""
    data = string.ascii_letters.encode()
    calls = []

    def short_fetcher(start, end):
        calls.append((start, end))
        real_end = min(end, 30)
        if real_end <= start:
            return b""
        return data[start:real_end]

    cache = AdaptiveReadAheadCache(8, short_fetcher, len(data))
    assert cache._fetch(28, 34) == data[28:30]
    # initial fetch plus a single retry confined to the unmet demand
    assert calls == [(28, 34), (30, 34)]
    assert cache.end == 30

    # demands past the source end return empty without whole-file fetches
    assert cache._fetch(40, 44) == b""
    assert all(start >= 40 and end - start <= 4 for start, end in calls[2:])


def test_adaptive_readahead_persistent_short_read_converges():
    """A source that always short-reads must not loop or read the whole file."""
    data = string.ascii_letters.encode()
    calls = []

    def always_short(start, end):
        calls.append((start, end))
        if start >= 10:
            return b""
        return data[start : min(end, 10)]

    cache = AdaptiveReadAheadCache(8, always_short, len(data))
    assert cache._fetch(8, 20) == data[8:10]
    assert len(calls) == 2  # exactly one retry, no speculation to EOF
    assert cache.end == 10


def test_readahead_short_read_policy_in_fixed_mode():
    data = string.ascii_letters.encode()
    calls = []

    def short_fetcher(start, end):
        calls.append((start, end))
        real_end = min(end, 12)
        return data[start:real_end] if real_end > start else b""

    cache = ReadAheadCache(8, short_fetcher, len(data))
    assert cache._fetch(8, 14) == data[8:12]
    assert calls == [(8, 22), (12, 14)]
    assert cache.end == 12


def test_adaptive_readahead_partial_hit_content():
    data = string.ascii_letters.encode()
    cache = AdaptiveReadAheadCache(10, letters_fetcher, len(data))
    # warm a window, then extend beyond it: the suffix must be stitched on
    assert cache._fetch(0, 10) == data[0:10]
    assert cache._fetch(5, 18) == data[5:18]
    assert cache._fetch(5, 18) == data[5:18]


def test_adaptive_readahead_stats_accounting():
    data = string.ascii_letters.encode()
    fetch, _ = _tracking_fetcher(data)
    cache = AdaptiveReadAheadCache(8, fetch, len(data))

    cache._fetch(0, 4)
    cache._fetch(4, 8)
    cache._fetch(8, 12)
    cache._fetch(0, 2)  # reverse jump discards the prefetched tail

    assert cache.demand_bytes + cache.prefetch_bytes == cache.total_requested_bytes
    assert 0 <= cache.wasted_prefetch_bytes <= cache.prefetch_bytes
    assert cache.hit_count + cache.miss_count == 4

    cache._reset_stats()
    assert (
        cache.hit_count,
        cache.miss_count,
        cache.total_requested_bytes,
        cache.demand_bytes,
        cache.prefetch_bytes,
        cache.wasted_prefetch_bytes,
    ) == (0, 0, 0, 0, 0, 0)


def test_adaptive_readahead_sequential_and_random_trace_parity():
    """Same requested content under both traces; fetch intervals are recorded."""
    data = string.ascii_letters.encode()
    sequential = [(i, min(i + 3, len(data))) for i in range(0, len(data), 3)]
    random = [(s, s + 2) for s in (40, 0, 20, 44, 4, 26, 10, 34, 16, 48)]

    def run(trace, adaptive):
        fetch, calls = _tracking_fetcher(data)
        if adaptive:
            cache = AdaptiveReadAheadCache(8, fetch, len(data))
        else:
            cache = ReadAheadCache(8, fetch, len(data))
        content = b"".join(cache._fetch(start, end) for start, end in trace)
        return content, calls

    expected_random = b"".join(data[s:e] for s, e in random)
    expected_sequential = b"".join(data[s:e] for s, e in sequential)

    random_content, random_calls = run(random, adaptive=True)
    sequential_content, sequential_calls = run(sequential, adaptive=True)
    fixed_content, fixed_calls = run(random, adaptive=False)

    # content is identical regardless of the caching strategy
    assert random_content == expected_random
    assert sequential_content == expected_sequential
    assert fixed_content == expected_random

    # adaptive random access fetches far fewer bytes than both sequential
    # adaptive access and the classic fixed readahead on the same trace
    def volume(intervals):
        return sum(end - start for start, end in intervals)

    assert volume(random_calls) < volume(sequential_calls)
    assert volume(random_calls) < volume(fixed_calls)


def test_adaptive_readahead_selected_in_open_flow():
    from fsspec.spec import AbstractBufferedFile

    data = string.ascii_letters.encode()

    class TestFile(AbstractBufferedFile):
        DEFAULT_BLOCK_SIZE = 8

        def _fetch_range(self, start, end):
            return data[start:end]

    with TestFile(
        None,
        "afile",
        mode="rb",
        cache_type="adaptive_readahead",
        size=len(data),
    ) as f:
        assert isinstance(f.cache, AdaptiveReadAheadCache)
        assert f.cache.adaptive is True
        assert f.read(12) == data[:12]


def test_bytes_cache_range_ending_at_cache_end():
    """
    A range that stops exactly where the cached data stops is already held in
    full, so it must be served without going back to the fetcher.
    """
    block_size = 5
    calls = []

    def counting_fetcher(start, end):
        calls.append((start, end))
        return letters_fetcher(start, end)

    cache = BytesCache(block_size, counting_fetcher, len(string.ascii_letters))

    assert cache._fetch(0, 5) == letters_fetcher(0, 5)
    assert cache.miss_count == 1
    assert cache.hit_count == 0
    # the read ahead leaves the cache holding [0, 10)
    assert calls == [(0, 10)]

    # ends exactly at the end of the cached range: every byte is already here
    assert cache._fetch(5, 10) == letters_fetcher(5, 10)
    assert cache.hit_count == 1
    assert cache.miss_count == 1
    assert calls == [(0, 10)]


def _fetcher(start, end):
    return b"0" * (end - start)


def letters_fetcher(start, end):
    return string.ascii_letters[start:end].encode()


def multi_letters_fetcher(ranges):
    return [string.ascii_letters[start:end].encode() for start, end in ranges]


not_parts_caches = {k: v for k, v in caches.items() if k != "parts"}


@pytest.fixture(params=not_parts_caches.values(), ids=list(not_parts_caches))
def Cache_imp(request):
    return request.param


def test_cache_empty_file(Cache_imp):
    blocksize = 5
    size = 0
    cache = Cache_imp(blocksize, _fetcher, size)
    assert cache._fetch(0, 0) == b""


def test_cache_fetch_past_end_of_file(Cache_imp):
    # Reading past the end of the file returns the available bytes rather than
    # raising: `read(n)` yields at most n bytes. BlockCache/BackgroundBlockCache
    # used to overflow their block count and raise ValueError here.
    blocksize = 5
    size = len(string.ascii_letters)
    cache = Cache_imp(blocksize, letters_fetcher, size)
    assert cache._fetch(0, size + 3 * blocksize) == string.ascii_letters.encode()


def test_cache_pickleable(Cache_imp):
    blocksize = 5
    size = 100
    cache = Cache_imp(blocksize, _fetcher, size)
    cache._fetch(0, 5)  # fill in cache
    payload = pickle.dumps(cache)
    assert cache._fetch(0, 10) == b"0" * 10
    unpickled = pickle.loads(payload)
    assert isinstance(unpickled, Cache_imp)
    assert unpickled.blocksize == blocksize
    assert unpickled.size == size
    assert unpickled._fetch(0, 10) == b"0" * 10


def test_first_cache_without_fetcher():
    c = FirstChunkCache(5, letters_fetcher, 52)
    assert c.cache is None
    assert c._fetch(12, 15) == letters_fetcher(12, 15)
    assert c.cache is None
    assert c._fetch(3, 10) == letters_fetcher(3, 10)
    assert c.cache == letters_fetcher(0, 5)
    c.fetcher = None
    assert c._fetch(1, 4) == letters_fetcher(1, 4)


@pytest.mark.parametrize("payload", [b"payload", b""], ids=["nonempty", "empty"])
@pytest.mark.parametrize("persistent", [False, True])
@pytest.mark.parametrize("close_mode", ["explicit", "context", "exception"])
def test_mmap_closes_with_file(payload, persistent, close_mode, tmp_path):
    fs = MemoryFileSystem()
    path = "/mmap-close-" + tmp_path.name
    fs.pipe_file(path, payload)
    stream = AbstractBufferedFile(
        fs,
        path,
        mode="rb",
        block_size=4,
        cache_type="mmap",
        cache_options={"location": str(tmp_path / "mapped") if persistent else None},
    )
    mapping = stream.cache.cache
    try:
        if close_mode == "explicit":
            assert stream.read() == payload
            stream.close()
        elif close_mode == "context":
            with stream:
                assert stream.read() == payload
        else:
            with pytest.raises(RuntimeError, match="stop"):
                with stream:
                    assert stream.read() == payload
                    raise RuntimeError("stop")
        assert stream.closed
        assert stream.cache is None
        if payload:
            assert mapping.closed
            if persistent:
                assert (tmp_path / "mapped").read_bytes() == payload
        stream.close()
    finally:
        stream.close()
        if payload:
            mapping.close()


def test_mmap_cache(mocker):
    fetcher = mocker.Mock(wraps=letters_fetcher)
    c = MMapCache(5, fetcher, 52)
    assert c._fetch(6, 8) == letters_fetcher(6, 8)
    assert fetcher.call_count == 1
    assert c._fetch(17, 22) == letters_fetcher(17, 22)
    assert fetcher.call_count == 2
    assert c._fetch(1, 38) == letters_fetcher(1, 38)
    assert fetcher.call_count == 5

    multi_fetcher = mocker.Mock(wraps=multi_letters_fetcher)
    m = MMapCache(5, fetcher, size=52, multi_fetcher=multi_fetcher)
    assert m._fetch(6, 8) == letters_fetcher(6, 8)
    assert multi_fetcher.call_count == 1
    assert m._fetch(17, 22) == letters_fetcher(17, 22)
    assert multi_fetcher.call_count == 2
    assert m._fetch(1, 38) == letters_fetcher(1, 38)
    assert multi_fetcher.call_count == 3
    assert fetcher.call_count == 5


@pytest.mark.parametrize("remove_location", [False, True])
def test_mmap_cache_pickle_named_location(tmp_path, remove_location):
    location = tmp_path / "cached"
    cache = MMapCache(5, letters_fetcher, 52, location=str(location))
    assert cache._fetch(6, 8) == letters_fetcher(6, 8)
    payload = pickle.dumps(cache)
    requested = cache.total_requested_bytes
    cache.cache.close()
    if remove_location:
        location.unlink()

    restored = pickle.loads(payload)
    try:
        assert restored._fetch(6, 8) == letters_fetcher(6, 8)
        assert restored.total_requested_bytes == requested + (
            5 if remove_location else 0
        )
    finally:
        restored.cache.close()


def test_mmap_cache_new_named_location_clears_shared_blocks(tmp_path):
    blocks = {1}
    cache = MMapCache(5, letters_fetcher, 52, str(tmp_path / "cached"), blocks)
    try:
        assert cache.blocks is blocks
        assert blocks == set()
        assert cache._fetch(6, 8) == letters_fetcher(6, 8)
        assert blocks == {1}
    finally:
        cache.cache.close()


def test_mmap_cache_http_reader_recreates_location(server, tmp_path):
    import fsspec

    location = tmp_path / "cached"
    fs = fsspec.filesystem(
        "http", headers={"head_ok": "true", "head_give_length": "true"}
    )
    with fs.open(
        server.realfile,
        block_size=5,
        cache_type="mmap",
        cache_options={"location": str(location)},
    ) as reader:
        expected = reader.read(2)
        blocks = reader.cache.blocks
        reader.cache.cache.close()
    location.unlink()

    with fs.open(
        server.realfile,
        block_size=5,
        cache_type="mmap",
        cache_options={"location": str(location), "blocks": blocks},
    ) as reader:
        try:
            assert reader.read(2) == expected
        finally:
            reader.cache.cache.close()


@pytest.mark.parametrize("use_multi_fetcher", [False, True])
@pytest.mark.parametrize("failed_start", [0, 8])
@pytest.mark.parametrize("short_read", [False, True])
def test_mmap_cache_retries_failed_ranges(use_multi_fetcher, failed_start, short_read):
    data = b"abcdefghijkl"
    calls = []
    failed = False

    def fetcher(start, end):
        nonlocal failed
        calls.append((start, end))
        if start == failed_start and not failed:
            failed = True
            if short_read:
                return b""
            raise OSError("temporary read failure")
        return data[start:end]

    def multi_fetcher(ranges):
        for start, end in ranges:
            yield fetcher(start, end)

    cache = MMapCache(
        4,
        fetcher,
        len(data),
        multi_fetcher=multi_fetcher if use_multi_fetcher else None,
    )
    try:
        # A cached middle block splits the next read into two missing ranges.
        assert cache._fetch(4, 7) == data[4:7]
        with pytest.raises(IndexError if short_read else OSError):
            cache._fetch(0, 11)

        assert cache.blocks == ({1} if failed_start == 0 else {0, 1})
        calls.clear()
        assert cache._fetch(0, 11) == data[:11]
        assert calls == ([(0, 4), (8, 12)] if failed_start == 0 else [(8, 12)])
        assert cache.blocks == {0, 1, 2}

        calls.clear()
        assert cache._fetch(0, 11) == data[:11]
        assert calls == []
    finally:
        cache.cache.close()


@pytest.mark.parametrize(
    "size_requests",
    [[(0, 30), (0, 35), (51, 52)], [(0, 1), (1, 11), (1, 52)], [(0, 52), (11, 15)]],
)
@pytest.mark.parametrize("blocksize", [1, 10, 52, 100])
def test_cache_basic(Cache_imp, blocksize, size_requests):
    cache = Cache_imp(blocksize, letters_fetcher, len(string.ascii_letters))

    for start, end in size_requests:
        result = cache._fetch(start, end)
        expected = string.ascii_letters[start:end].encode()
        assert result == expected


@pytest.mark.parametrize("strict", [True, False])
def test_known_read_starting_outside_a_part(strict):
    parts = {(0, 10): b"0" * 10, (40, 50): b"3" * 10}
    c = caches["parts"](None, None, 100, parts, strict=strict)

    # a read that starts in the gap has no first byte to return, in either mode
    with pytest.raises(ValueError):
        c._fetch(20, 30)
    with pytest.raises(ValueError):
        c._fetch(35, 45)

    # a read that starts inside a part is unaffected
    assert c._fetch(5, 10) == b"0" * 5


@pytest.mark.parametrize("strict", [True, False])
@pytest.mark.parametrize("sort", [True, False])
def test_known(strict, sort):
    parts = {
        (10, 20): b"1" * 10,
        (20, 30): b"2" * 10,
        (0, 10): b"0" * 10,
        (40, 50): b"3" * 10,
    }
    if sort:
        parts = dict(sorted(parts.items()))
    c = caches["parts"](None, None, 100, parts, strict=strict)
    assert c.size == 40
    assert (0, 30) in c.data  # got consolidated
    assert c.nblocks == 2

    assert c._fetch(5, 15) == b"0" * 5 + b"1" * 5
    assert c._fetch(15, 25) == b"1" * 5 + b"2" * 5
    assert c.hit_count
    assert not c.miss_count

    if strict:
        # Over-read will raise error
        with pytest.raises(ValueError):
            c._fetch(25, 35)
        with pytest.raises(ValueError):
            c._fetch(25, 45)
    else:
        assert c._fetch(25, 35) == b"2" * 5 + b"\x00" * 5
        assert c._fetch(25, 45) == b"2" * 5 + b"\x00" * 10 + b"3" * 5
    assert c.miss_count


@pytest.mark.parametrize("strict", [True, False])
def test_known_parts_read_beginning_outside_a_part(strict):
    """
    A read that begins outside every known byte range cannot be served: there
    are no bytes to return and, per the ``strict`` docstring, such a read is
    not zero padded either, so it must raise instead of quietly handing back
    bytes from the wrong offsets.
    """
    parts = {
        (10, 20): b"1" * 10,
        (20, 30): b"2" * 10,
    }
    c = caches["parts"](None, None, 100, parts, strict=strict)
    assert c.data == {(10, 30): b"1" * 10 + b"2" * 10}  # got consolidated

    # these used to be answered with bytes taken from the wrong offsets
    for start, stop in [(5, 15), (5, 25), (9, 11)]:
        hits = c.hit_count
        with pytest.raises(ValueError):
            c._fetch(start, stop)
        assert c.hit_count == hits, "a read that cannot be served is not a hit"

    # a read reaching past the end of the known data still fails
    with pytest.raises(ValueError):
        c._fetch(5, 100)

    # reads that do begin inside a part are unaffected
    assert c._fetch(12, 18) == b"1" * 6
    assert c._fetch(25, 30) == b"2" * 5


def test_background(server, monkeypatch):
    import threading
    import time

    import fsspec

    head = {"head_ok": "true", "head_give_length": "true"}
    urla = server.realfile
    h = fsspec.filesystem("http", headers=head)
    thread_ids = {threading.current_thread().ident}
    f = h.open(urla, block_size=5, cache_type="background")
    orig = f.cache._fetch_block

    def wrapped(*a, **kw):
        thread_ids.add(threading.current_thread().ident)
        return orig(*a, **kw)

    f.cache._fetch_block = wrapped
    assert len(thread_ids) == 1
    f.read(1)
    time.sleep(0.1)  # second block is loading
    assert len(thread_ids) == 2


def test_background_shutdown_on_close():
    import weakref

    from fsspec.spec import AbstractBufferedFile

    data = b"abcdefgh"

    class TestFile(AbstractBufferedFile):
        DEFAULT_BLOCK_SIZE = 4

        def _fetch_range(self, start, end):
            return data[start:end]

    f = TestFile(
        None,
        "test",
        mode="rb",
        cache_type="background",
        size=len(data),
    )
    f.read(1)
    cache = f.cache
    assert isinstance(cache, BackgroundBlockCache)
    cache_ref = weakref.ref(cache)
    executor = cache._thread_executor
    del cache

    f.close()

    with pytest.raises(RuntimeError, match="cannot schedule new futures"):
        executor.submit(lambda: None)
    assert cache_ref() is None


def test_register_cache():
    # just test that we have them populated and fail to re-add again unless overload
    with pytest.raises(ValueError):
        register_cache(BlockCache)
    register_cache(BlockCache, clobber=True)


def test_cache_kwargs(mocker):
    # test that kwargs are passed to the underlying filesystem after cache commit

    fs = WholeFileCacheFileSystem(target_protocol="memory")
    fs.touch("test")
    fs.fs.put = mocker.MagicMock()

    with fs.open("test", "wb", overwrite=True) as file_handle:
        file_handle.write(b"foo")

    # We don't care about the first parameter, just retrieve its expected value.
    # It is a random location that cannot be predicted.
    # The important thing is the 'overwrite' kwarg
    fs.fs.put.assert_called_with(fs.fs.put.call_args[0][0], ["/test"], overwrite=True)


def test_adaptive_cache_with_async_fetcher():
    data = string.ascii_letters.encode()

    cache = caches["adaptive"](
        8,
        letters_fetcher,
        len(data),
        # Keep this below the default prefetch minimum so behavior is deterministic
        # and bounded for unit testing.
        concurrency=2,
        max_prefetch_size=64,
    )
    try:
        assert cache._fetch(0, 0) == b""
        assert cache._fetch(0, 5) == data[0:5]
        assert cache._fetch(5, 12) == data[5:12]
        assert cache._fetch(12, 20) == data[12:20]
    finally:
        cache.close()


def test_adaptive_cache_registered():
    assert "adaptive" in caches


def test_adaptive_cache_fallback_when_prefetcher_init_fails(monkeypatch):
    import fsspec.prefetcher as prefetcher_mod

    class FailingPrefetcher:
        def __init__(self, *args, **kwargs):
            raise RuntimeError("prefetch init failed")

    monkeypatch.setattr(prefetcher_mod, "BackgroundPrefetcher", FailingPrefetcher)

    data = string.ascii_letters.encode()
    cache = caches["adaptive"](8, letters_fetcher, len(data))

    # On prefetch setup failure, adaptive must still serve reads through readahead.
    assert cache._fetch(0, 10) == data[0:10]
    assert cache._fetch(10, 17) == data[10:17]
    assert cache._prefetcher is None


def test_adaptive_cache_selected_in_open_flow():
    from fsspec.spec import AbstractBufferedFile

    data = string.ascii_letters.encode()

    class TestFile(AbstractBufferedFile):
        DEFAULT_BLOCK_SIZE = 8

        def _fetch_range(self, start, end):
            return data[start:end]

    with TestFile(None, "afile", mode="rb", cache_type="adaptive", size=len(data)) as f:
        assert f.cache.name == "adaptive"
        assert f.read(12) == data[:12]


def test_adaptive_cache_fallback_without_loop():
    data = string.ascii_letters.encode()
    cache = caches["adaptive"](8, letters_fetcher, len(data))

    assert cache._fetch(0, 10) == data[0:10]
    assert cache._fetch(10, 17) == data[10:17]


@pytest.mark.parametrize("size", [0, 3, 8])
@pytest.mark.parametrize("warm", [False, True])
def test_first_cache_eof_does_not_fetch(size, warm):
    data = b"abcdefgh"[:size]
    requests = []

    def fetch(start, end):
        requests.append((start, end))
        return data[start:end]

    cache = FirstChunkCache(4, fetch, size)
    if warm and size:
        assert cache._fetch(0, size) == data
    before = requests.copy()
    counts = (cache.hit_count, cache.miss_count, cache.total_requested_bytes)
    for end in (None, size, size + 1, size + 10):
        assert cache._fetch(size, end) == b""
    assert requests == before
    assert (cache.hit_count, cache.miss_count, cache.total_requested_bytes) == counts


def test_first_cache_buffered_gzip_eof():
    import gzip

    payload = b"example user data\n" * 100
    compressed = gzip.compress(payload)
    fs = MemoryFileSystem()
    fs.pipe("/first-cache-eof.gz", compressed)
    with AbstractBufferedFile(
        fs, "/first-cache-eof.gz", block_size=16, cache_type="first"
    ) as f:
        requests = []
        fetch = f.cache.fetcher

        def tracked_fetch(start, end):
            requests.append((start, end))
            return fetch(start, end)

        f.cache.fetcher = tracked_fetch
        with gzip.GzipFile(fileobj=f) as z:
            assert z.read() == payload
        assert all(start < end for start, end in requests)
        before = requests.copy()
        assert f.read(1) == b""
        assert f.read(10) == b""
        assert requests == before
