# -*- coding: utf-8 -*-
from __future__ import absolute_import, print_function, division


import glob
from tempfile import NamedTemporaryFile


import pytest


from petl.errors import ArgumentError
from petl.test.helpers import ieq
import petl as etl


def test_teepickle():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)
    etl.wrap(t1).teepickle(f1.name).selectgt('bar', 1).topickle(f2.name)

    ieq(t1, etl.frompickle(f1.name))
    ieq(etl.wrap(t1).selectgt('bar', 1), etl.frompickle(f2.name))


def test_teecsv():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    (etl
     .wrap(t1)
     .teecsv(f1.name, encoding='ascii')
     .selectgt('bar', 1)
     .tocsv(f2.name, encoding='ascii'))

    ieq(t1,
        etl.fromcsv(f1.name, encoding='ascii').convertnumbers())
    ieq(etl.wrap(t1).selectgt('bar', 1),
        etl.fromcsv(f2.name, encoding='ascii').convertnumbers())


def test_teetsv():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    (etl
     .wrap(t1)
     .teetsv(f1.name, encoding='ascii')
     .selectgt('bar', 1)
     .totsv(f2.name, encoding='ascii'))

    ieq(t1,
        etl.fromtsv(f1.name, encoding='ascii').convertnumbers())
    ieq(etl.wrap(t1).selectgt('bar', 1),
        etl.fromtsv(f2.name, encoding='ascii').convertnumbers())


def test_teecsv_write_header():

    t1 = (('foo', 'bar'),
          ('a', '2'),
          ('b', '1'),
          ('c', '3'))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    (etl
     .wrap(t1)
     .convertnumbers()
     .teecsv(f1.name, write_header=False, encoding='ascii')
     .selectgt('bar', 1)
     .tocsv(f2.name, encoding='ascii'))

    ieq(t1[1:],
        etl.fromcsv(f1.name, encoding='ascii'))
    ieq(etl.wrap(t1).convertnumbers().selectgt('bar', 1),
        etl.fromcsv(f2.name, encoding='ascii').convertnumbers())


def test_teecsv_unicode():

    t1 = ((u'name', u'id'),
          (u'Արամ Խաչատրյան', 1),
          (u'Johann Strauß', 2),
          (u'Вагиф Сәмәдоғлу', 3),
          (u'章子怡', 4))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    (etl
     .wrap(t1)
     .teecsv(f1.name, encoding='utf-8')
     .selectgt('id', 1)
     .tocsv(f2.name, encoding='utf-8'))

    ieq(t1,
        etl.fromcsv(f1.name, encoding='utf-8').convertnumbers())
    ieq(etl.wrap(t1).selectgt('id', 1),
        etl.fromcsv(f2.name, encoding='utf-8').convertnumbers())


def test_teecsv_unicode_write_header():

    t1 = ((u'name', u'id'),
          (u'Արամ Խաչատրյան', u'1'),
          (u'Johann Strauß', u'2'),
          (u'Вагиф Сәмәдоғлу', u'3'),
          (u'章子怡', u'4'))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    (etl
     .wrap(t1)
     .convertnumbers()
     .teecsv(f1.name, write_header=False, encoding='utf-8')
     .selectgt('id', 1)
     .tocsv(f2.name, encoding='utf-8'))

    ieq(t1[1:],
        etl.fromcsv(f1.name, encoding='utf-8'))
    ieq(etl.wrap(t1).convertnumbers().selectgt('id', 1),
        etl.fromcsv(f2.name, encoding='utf-8').convertnumbers())


def test_teetsv_unicode():

    t1 = ((u'name', u'id'),
          (u'Արամ Խաչատրյան', 1),
          (u'Johann Strauß', 2),
          (u'Вагиф Сәмәдоғлу', 3),
          (u'章子怡', 4),)

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    (etl
     .wrap(t1)
     .teetsv(f1.name, encoding='utf-8')
     .selectgt('id', 1)
     .totsv(f2.name, encoding='utf-8'))

    ieq(t1,
        etl.fromtsv(f1.name, encoding='utf-8').convertnumbers())
    ieq(etl.wrap(t1).selectgt('id', 1),
        etl.fromtsv(f2.name, encoding='utf-8').convertnumbers())


def test_teetext():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    prologue = 'foo,bar\n'
    template = '{foo},{bar}\n'
    epilogue = 'd,4'
    (etl
     .wrap(t1)
     .teetext(f1.name,
              template=template,
              prologue=prologue,
              epilogue=epilogue)
     .selectgt('bar', 1)
     .topickle(f2.name))

    ieq(t1 + (('d', 4),),
        etl.fromcsv(f1.name).convertnumbers())
    ieq(etl.wrap(t1).selectgt('bar', 1),
        etl.frompickle(f2.name))


def test_teetext_unicode():

    t1 = ((u'foo', u'bar'),
          (u'Արամ Խաչատրյան', 2),
          (u'Johann Strauß', 1),
          (u'Вагиф Сәмәдоғлу', 3))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)

    prologue = u'foo,bar\n'
    template = u'{foo},{bar}\n'
    epilogue = u'章子怡,4'
    (etl
     .wrap(t1)
     .teetext(f1.name,
              template=template,
              prologue=prologue,
              epilogue=epilogue,
              encoding='utf-8')
     .selectgt('bar', 1)
     .topickle(f2.name))

    ieq(t1 + ((u'章子怡', 4),),
        etl.fromcsv(f1.name, encoding='utf-8').convertnumbers())
    ieq(etl.wrap(t1).selectgt('bar', 1),
        etl.frompickle(f2.name))


def test_teehtml():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)
    etl.wrap(t1).teehtml(f1.name).selectgt('bar', 1).topickle(f2.name)

    ieq(t1, etl.fromxml(f1.name, './/tr', ('th', 'td')).convertnumbers())
    ieq(etl.wrap(t1).selectgt('bar', 1), etl.frompickle(f2.name))


def test_teehtml_unicode():

    t1 = ((u'foo', u'bar'),
          (u'Արամ Խաչատրյան', 2),
          (u'Johann Strauß', 1),
          (u'Вагиф Сәмәдоғлу', 3))

    f1 = NamedTemporaryFile(delete=False)
    f2 = NamedTemporaryFile(delete=False)
    (etl
     .wrap(t1)
     .teehtml(f1.name, encoding='utf-8')
     .selectgt('bar', 1)
     .topickle(f2.name))

    ieq(t1,
        (etl
         .fromxml(f1.name, './/tr', ('th', 'td'), encoding='utf-8')
         .convertnumbers()))
    ieq(etl.wrap(t1).selectgt('bar', 1), etl.frompickle(f2.name))


def _atomic_target(content=None):
    f = NamedTemporaryFile(delete=False)
    if content is not None:
        f.write(content.encode('utf-8'))
    f.close()
    return f.name


def _leftover_temps(target):
    return glob.glob(target + '.*.tmp')


def test_teecsv_atomic_complete():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    target = _atomic_target('sentinel,content\nto,replace\n')

    view = etl.wrap(t1).teecsv(target, encoding='ascii', atomic=True)
    ieq(t1, view)

    # target replaced with the full contents, no temporary files left
    ieq(t1, etl.fromcsv(target, encoding='ascii').convertnumbers())
    assert _leftover_temps(target) == []

    # a second pass over the same view has an independent write lifecycle
    ieq(t1, view)
    ieq(t1, etl.fromcsv(target, encoding='ascii').convertnumbers())
    assert _leftover_temps(target) == []


def test_teecsv_atomic_abort():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    target = _atomic_target('sentinel,content\n')

    view = etl.wrap(t1).teecsv(target, encoding='ascii', atomic=True)
    it = iter(view)
    next(it)
    next(it)
    it.close()

    # existing target untouched, temporary file cleaned up
    with open(target) as f:
        assert f.read() == 'sentinel,content\n'
    assert _leftover_temps(target) == []


def test_teecsv_atomic_source_error():

    class BrokenTable(object):
        def __iter__(self):
            yield ('foo', 'bar')
            yield ('a', 1)
            raise ValueError('boom')

    target = _atomic_target('sentinel,content\n')

    view = etl.teecsv(BrokenTable(), target, encoding='ascii', atomic=True)
    with pytest.raises(ValueError):
        list(view)

    with open(target) as f:
        assert f.read() == 'sentinel,content\n'
    assert _leftover_temps(target) == []


def test_teecsv_atomic_concurrent_iterators():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    target = _atomic_target()
    view = etl.wrap(t1).teecsv(target, encoding='ascii', atomic=True)

    it1 = iter(view)
    it2 = iter(view)
    assert next(it1) == t1[0]
    assert next(it2) == t1[0]

    # two simultaneous iterators use independent temporary files
    assert len(_leftover_temps(target)) == 2

    assert list(it1) == list(t1[1:])
    assert list(it2) == list(t1[1:])

    ieq(t1, etl.fromcsv(target, encoding='ascii').convertnumbers())
    assert _leftover_temps(target) == []


def test_teecsv_atomic_invalid_source():

    t1 = (('foo', 'bar'),
          ('a', 2))

    # stdout is not a local file, atomic publishing is not possible
    view = etl.wrap(t1).teecsv(atomic=True)
    with pytest.raises(ArgumentError):
        list(view)


def test_teetext_atomic_complete():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    target = _atomic_target('sentinel content\n')

    prologue = 'foo,bar\n'
    template = '{foo},{bar}\n'
    epilogue = 'd,4'
    view = etl.wrap(t1).teetext(target, template=template, prologue=prologue,
                                epilogue=epilogue, atomic=True)
    ieq(t1, view)

    ieq(t1 + (('d', 4),), etl.fromcsv(target).convertnumbers())
    assert _leftover_temps(target) == []


def test_teetext_atomic_abort():

    t1 = (('foo', 'bar'),
          ('a', 2),
          ('b', 1),
          ('c', 3))

    target = _atomic_target('sentinel content\n')

    view = etl.wrap(t1).teetext(target, template='{foo},{bar}\n', atomic=True)
    it = iter(view)
    next(it)
    next(it)
    it.close()

    with open(target) as f:
        assert f.read() == 'sentinel content\n'
    assert _leftover_temps(target) == []


def test_teejson():

    t1 = (('foo', 'bar'),
          ('a', 1),
          ('b', 2),
          ('c', 2))

    f1 = NamedTemporaryFile(delete=False)
    f1.close()

    view = etl.wrap(t1).teejson(f1.name)
    ieq(t1, view)
    ieq(t1, etl.fromjson(f1.name, header=['foo', 'bar']))


def test_teejson_lines():

    t1 = (('foo', 'bar'),
          ('a', 1),
          ('b', 2),
          ('c', 2))

    f1 = NamedTemporaryFile(delete=False)
    f1.close()

    view = etl.wrap(t1).teejson(f1.name, lines=True)
    ieq(t1, view)
    ieq(t1, etl.fromjson(f1.name, header=['foo', 'bar'], lines=True))


def test_teejson_atomic_complete():

    t1 = (('foo', 'bar'),
          ('a', 1),
          ('b', 2),
          ('c', 2))

    target = _atomic_target('[{"sentinel": true}]')

    view = etl.wrap(t1).teejson(target, atomic=True)
    ieq(t1, view)

    ieq(t1, etl.fromjson(target, header=['foo', 'bar']))
    assert _leftover_temps(target) == []


def test_teejson_atomic_lines_abort():

    t1 = (('foo', 'bar'),
          ('a', 1),
          ('b', 2),
          ('c', 2))

    target = _atomic_target('{"sentinel": true}\n')

    view = etl.wrap(t1).teejson(target, lines=True, atomic=True)
    it = iter(view)
    next(it)
    next(it)
    it.close()

    with open(target) as f:
        assert f.read() == '{"sentinel": true}\n'
    assert _leftover_temps(target) == []


def test_teejson_atomic_serialisation_error():

    t1 = (('foo', 'bar'),
          ('a', 1),
          ('b', object()))

    target = _atomic_target('[{"sentinel": true}]')

    view = etl.wrap(t1).teejson(target, atomic=True)
    with pytest.raises(TypeError):
        list(view)

    with open(target) as f:
        assert f.read() == '[{"sentinel": true}]'
    assert _leftover_temps(target) == []
