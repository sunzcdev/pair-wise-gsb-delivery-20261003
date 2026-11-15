import pytest

from requests_cache.models import VaryFamily, normalize_vary


@pytest.mark.parametrize(
    'value, expected',
    [
        (None, ()),
        ('', ()),
        ('Accept-Language', ('accept-language',)),
        ('accept-language, accept-encoding', ('accept-encoding', 'accept-language')),
        ('Accept, accept', ('accept',)),
        ('  X , y ', ('x', 'y')),
        # A wildcard always wins and is never mixed with concrete header names
        ('*', ('*',)),
        ('Accept, *', ('*',)),
        ('*, Accept-Language', ('*',)),
    ],
)
def test_normalize_vary(value, expected):
    assert normalize_vary(value) == expected


def test_vary_family_tracks_members():
    family = VaryFamily(vary=('accept-language',), anchor_key='base', member_keys=['base'])
    assert not family.is_wildcard

    family.add('secondary')
    family.add('secondary')  # duplicates are ignored
    assert family.member_keys == ['base', 'secondary']
    assert family.contains('secondary')
    assert not family.contains('missing')


def test_vary_family_wildcard():
    family = VaryFamily(vary=('*',), anchor_key='base', member_keys=['base'])
    assert family.is_wildcard
