"""Thread-safety integration tests for concurrent detect() calls."""

from __future__ import annotations

import threading

import pytest

from chardet import detect
from chardet.enums import EncodingEra

# Acceptable encoding sets for each test sample.
# Names use canonical codec names (codecs.lookup(name).name) since the
# worker calls detect() with compat_names=False.
_JAPANESE_ENCODINGS = frozenset({"shift_jis_2004", "cp932"})
_GERMAN_ENCODINGS = frozenset(
    {"cp1252", "iso8859-1", "iso8859-15", "iso8859-9", "cp1254"}
)
_CHINESE_ENCODINGS = frozenset({"gb18030"})

# Test data shared across tests.
_JAPANESE = "これはテストです。日本語のテキスト。".encode("shift_jis")
_GERMAN = (
    "Die Größe des Gebäudes überraschte die Besucher. Natürlich können wir das ändern."
).encode("windows-1252")
_CHINESE = "这是中文测试文本，用于并发检测。".encode("gb18030")

_SAMPLES: list[tuple[bytes, frozenset[str]]] = [
    (_JAPANESE, _JAPANESE_ENCODINGS),
    (_GERMAN, _GERMAN_ENCODINGS),
    (_CHINESE, _CHINESE_ENCODINGS),
]


def _run_concurrent_detect(
    n_workers: int,
    iterations: int,
) -> list[str]:
    """Spawn *n_workers* threads per sample, each calling detect() *iterations* times.

    Returns a list of error strings (empty = success).
    """
    errors: list[str] = []
    barrier = threading.Barrier(n_workers * len(_SAMPLES))

    def worker(data: bytes, expected: frozenset[str]) -> None:
        barrier.wait()
        for _ in range(iterations):
            result = detect(data, encoding_era=EncodingEra.ALL, compat_names=False)
            enc = result["encoding"]
            if enc not in expected:
                errors.append(f"Expected one of {expected}, got {enc!r}")

    threads = []
    for _ in range(n_workers):
        for data, expected in _SAMPLES:
            t = threading.Thread(target=worker, args=(data, expected))
            threads.append(t)
            t.start()

    for t in threads:
        t.join()

    return errors


def test_concurrent_detect_no_corruption():
    """Multiple threads calling detect() simultaneously must not corrupt results."""
    errors = _run_concurrent_detect(n_workers=3, iterations=20)
    assert not errors, "Thread-safety violations:\n" + "\n".join(errors[:10])


def test_concurrent_detect_high_concurrency():
    """Stress test with higher thread count to surface free-threading races."""
    errors = _run_concurrent_detect(n_workers=8, iterations=10)
    assert not errors, "Thread-safety violations:\n" + "\n".join(errors[:10])


@pytest.mark.serial
def test_cold_cache_concurrent_init():
    """Race on first-call cache initialization from a cold state.

    Clears all functools.cache-backed caches, then has many threads
    simultaneously call detect().  This stresses the cache population
    path — the most dangerous codepath for thread safety.

    Marked ``serial`` because the global cache mutations are not safe to
    run concurrently with other tests that call ``detect()``.
    """
    import chardet.models as _models  # noqa: PLC0415
    import chardet.pipeline.confusion as _confusion  # noqa: PLC0415
    import chardet.registry as _registry  # noqa: PLC0415

    try:
        # Clear all caches to cold state.
        _models._load_models_data.cache_clear()
        _models.get_enc_index.cache_clear()
        _confusion._modelled_languages.cache_clear()
        _confusion.load_confusion_data.cache_clear()
        _registry.lookup_encoding.cache_clear()
        _registry.get_candidates.cache_clear()

        errors = _run_concurrent_detect(n_workers=6, iterations=5)
        assert not errors, "Cold-cache race violations:\n" + "\n".join(errors[:10])
    finally:
        # Clear and let caches re-populate naturally on next use.
        _models._load_models_data.cache_clear()
        _models.get_enc_index.cache_clear()
        _confusion._modelled_languages.cache_clear()
        _confusion.load_confusion_data.cache_clear()
        _registry.lookup_encoding.cache_clear()
        _registry.get_candidates.cache_clear()


@pytest.mark.serial
def test_lazy_loaders_single_flight_under_contention():
    """The lazy loaders publish one result no matter how many threads race.

    Every concurrent first call must collapse onto a single loader
    invocation per resource, and all waiters must receive the exact same
    published object rather than racing on half-built tables.  Repeated
    cold-cache rounds exercise the failure-forgetting retry boundary too:
    a reset between rounds must leave the loader usable again.
    """
    import chardet.models as _models  # noqa: PLC0415

    loaders = (_models._load_models_data, _models.get_enc_index)

    def round_once() -> list[str]:
        round_errors: list[str] = []
        barrier = threading.Barrier(8)
        results: list[object] = []

        def worker() -> None:
            barrier.wait()
            try:
                for loader in loaders:
                    results.append(loader())
            except Exception as exc:  # reported, not swallowed by the thread
                round_errors.append(repr(exc))

        threads = [threading.Thread(target=worker) for _ in range(8)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()

        # One result per (thread, loader); they must all be the same two
        # objects because each loader ran exactly once.
        if len(results) != 16 or len({id(item) for item in results}) != 2:
            round_errors.append(
                f"expected two shared objects across 16 gets, got {results!r}"
            )
        return round_errors

    try:
        for _ in range(3):
            for loader in loaders:
                loader.cache_clear()
            errors = round_once()
            assert not errors, "Lazy single-flight violations:\n" + "\n".join(errors)
    finally:
        for loader in loaders:
            loader.cache_clear()
