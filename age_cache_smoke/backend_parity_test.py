import time_machine
from datetime import datetime, timedelta, timezone
from pathlib import Path

from requests_cache.backends import BaseCache, SQLiteCache
from requests_cache.models import CachedResponse
from requests_cache.policy import get_age_adjusted_expiration, get_corrected_initial_age


HEADERS = {
    'Cache-Control': 'max-age=600',
    'Age': '300',
    'Date': 'Sun, 15 Nov 2026 12:00:00 GMT',
}
T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)


def _populate(cache):
    with time_machine.travel(T0):
        response = CachedResponse(
            url='http://example.com/data',
            status_code=200,
            headers=dict(HEADERS),
        )
        response.expires = get_age_adjusted_expiration(HEADERS, 600)
        response.age_at_creation = get_corrected_initial_age(HEADERS)
        cache.responses['same-key'] = response


def test_memory_and_sqlite_report_identical_remaining_lifetime():
    db_path = Path(__file__).parent / 'parity_unique.db'
    memory = BaseCache('memory-parity')
    sqlite = SQLiteCache(str(db_path))
    _populate(memory)
    _populate(sqlite)

    for offset in (0, 59, 180, 300, 301):
        with time_machine.travel(T0 + timedelta(seconds=offset)):
            mem_entry = memory.get_response('same-key')
            sql_entry = sqlite.get_response('same-key')
            assert mem_entry.age == sql_entry.age == 300 + offset
            assert mem_entry.expires_delta == sql_entry.expires_delta
            assert mem_entry.is_expired == sql_entry.is_expired
            assert mem_entry.expires == sql_entry.expires

    sqlite.close()
    sqlite.responses.close()
    for suffix in ('', '-wal', '-shm'):
        sidecar = Path(str(db_path) + suffix)
        if sidecar.exists():
            sidecar.unlink()
    if db_path.exists():
        db_path.unlink()

