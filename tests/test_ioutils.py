import io
import os
import codecs
import random
import string

from tempfile import mkdtemp
from unittest import TestCase
from zipfile import ZipFile, ZIP_DEFLATED


from boltons import ioutils

CUR_FILE_PATH = os.path.abspath(__file__)


class AssertionsMixin:

    def assertIsNone(self, item, msg=None):
        self.assertTrue(item is None, msg)


class BaseTestMixin:
    """
    A set of tests that work the same for SpooledBtyesIO and SpooledStringIO
    """

    def test_getvalue_norollover(self):
        """Make sure getvalue function works with in-memory flo"""
        self.spooled_flo.write(self.test_str)
        self.assertEqual(self.spooled_flo.getvalue(), self.test_str)

    def test_getvalue_rollover(self):
        """Make sure getvalue function works with on-disk flo"""
        self.spooled_flo.write(self.test_str)
        self.assertFalse(self.spooled_flo._rolled)
        self.spooled_flo.rollover()
        self.assertEqual(self.spooled_flo.getvalue(), self.test_str)
        self.assertTrue(self.spooled_flo._rolled)

    def test_rollover_custom_directory(self):
        """dir keyword argument is passed to TemporaryFile instantiation"""
        custom_dir = mkdtemp()
        try:
            # Re-instantiate self.spooled_flo with the custom dir argument
            _spooled_flo = type(self.spooled_flo)(dir=custom_dir)
            self.assertEqual(_spooled_flo._dir, custom_dir)
            # TemporaryFile is kind of a black box, we can't really test it
            # since the directory entry for the file is removed immediately
            # after the file is created. So we can't check path using fd.name
            # or listdir(custom_dir). We could either convert rollover() to
            # use NamedtemporaryFile-s or assume it's well tested enough that
            # passing dir= into the constructor will work as expected. We'll
            # call rollover() with the dir attribute set just to ensure
            # nothing has gone absurdly wrong.
            _spooled_flo.write(self.test_str)
            _spooled_flo.rollover()
            self.assertEqual(_spooled_flo.getvalue(), self.test_str)
            self.assertTrue(_spooled_flo._rolled)
            _spooled_flo.close()
        finally:
            os.rmdir(custom_dir)

    def test_compare_err(self):
        """Read-heads are reset if a comparison raises an error."""
        def _monkey_err(*args, **kwargs):
            raise Exception('A sad error has occurred today')

        a = self.spooled_flo.__class__()
        a.write(self.test_str)
        b = self.spooled_flo.__class__()
        b.write(self.test_str)

        a.seek(1)
        b.seek(2)

        b.__next__ = _monkey_err

        try:
            a == b
        except Exception:
            pass

        self.assertEqual(a.tell(), 1)
        self.assertEqual(b.tell(), 2)

    def test_truncate_noargs_norollover(self):
        """Test truncating with no args with in-memory flo"""
        self.spooled_flo.write(self.test_str)
        self.spooled_flo.seek(10)
        self.spooled_flo.truncate()
        self.assertEqual(self.spooled_flo.getvalue(), self.test_str[:10])

    def test_truncate_noargs_rollover(self):
        """Test truncating with no args with on-disk flo"""
        self.spooled_flo.write(self.test_str)
        self.spooled_flo.seek(10)
        self.spooled_flo.rollover()
        self.spooled_flo.truncate()
        self.assertEqual(self.spooled_flo.getvalue(), self.test_str[:10])

    def test_truncate_with_args_norollover(self):
        """Test truncating to a value with in-memory flo"""
        self.spooled_flo.write(self.test_str)
        self.spooled_flo.seek(5)
        self.spooled_flo.truncate(10)
        self.assertEqual(self.spooled_flo.getvalue(), self.test_str[:10])

    def test_truncate_with_args_rollover(self):
        """Test truncating to a value with on-disk flo"""
        self.spooled_flo.write(self.test_str)
        self.spooled_flo.seek(5)
        self.spooled_flo.rollover()
        self.spooled_flo.truncate(10)
        self.assertEqual(self.spooled_flo.getvalue(), self.test_str[:10])

    def test_truncate_keeps_position(self):
        """truncate() never moves the position, like io.IOBase"""
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = type(self.spooled_flo)()
                flo.write(self.test_str)
                if rolled:
                    flo.rollover()
                flo.seek(15)
                flo.truncate(10)
                self.assertEqual(flo.tell(), 15)
                self.assertEqual(flo.getvalue(), self.test_str[:10])

    def test_type_error_too_many_args(self):
        """Make sure TypeError raised if too many args passed to truncate"""
        self.spooled_flo.write(self.test_str)
        self.assertRaises(TypeError, self.spooled_flo.truncate, 0, 10)

    def test_io_error_negative_truncate(self):
        """Make sure IOError raised trying to truncate with negative value"""
        self.spooled_flo.write(self.test_str)
        self.assertRaises(IOError, self.spooled_flo.truncate, -1)

    def test_compare_different_instances(self):
        """Make sure two different instance types are not considered equal"""
        a = ioutils.SpooledBytesIO()
        a.write(b"I am equal!")
        b = ioutils.SpooledStringIO()
        b.write("I am equal!")
        self.assertNotEqual(a, b)

    def test_compare_unequal_instances(self):
        """Comparisons of non-SpooledIOBase classes should fail"""
        self.assertNotEqual("Bummer dude", self.spooled_flo)

    def test_set_softspace_attribute(self):
        """Ensure softspace attribute can be retrieved and set"""
        self.spooled_flo.softspace = True
        self.assertTrue(self.spooled_flo.softspace)

    def test_set_softspace_attribute_rolled(self):
        """Ensure softspace attribute can be retrieved and set if rolled"""
        self.spooled_flo.softspace = True
        self.assertTrue(self.spooled_flo.softspace)
        self.spooled_flo.rollover()
        self.spooled_flo.softspace = True
        self.assertTrue(self.spooled_flo.softspace)

    def test_buf_property(self):
        """'buf' property returns the same value as getvalue()"""
        self.assertEqual(self.spooled_flo.buf, self.spooled_flo.getvalue())

    def test_pos_property(self):
        """'pos' property returns the same value as tell()"""
        self.assertEqual(self.spooled_flo.pos, self.spooled_flo.tell())

    def test_closed_property(self):
        """'closed' property works as expected"""
        self.assertFalse(self.spooled_flo.closed)
        self.spooled_flo.close()
        self.assertTrue(self.spooled_flo.closed)

    def test_readline(self):
        """Make readline returns expected values"""
        self.spooled_flo.write(self.test_str_lines)
        self.spooled_flo.seek(0)
        self.assertEqual(self.spooled_flo.readline().rstrip(self.linesep),
                         self.test_str_lines.split(self.linesep)[0])

    def test_readlines(self):
        """Make sure readlines returns expected values"""
        self.spooled_flo.write(self.test_str_lines)
        self.spooled_flo.seek(0)
        self.assertEqual(
            [x.rstrip(self.linesep) for x in self.spooled_flo.readlines()],
            self.test_str_lines.split(self.linesep)
        )

    def test_next(self):
        """Make next returns expected values"""
        self.spooled_flo.write(self.test_str_lines)
        self.spooled_flo.seek(0)
        self.assertEqual(self.spooled_flo.next().rstrip(self.linesep),
                         self.test_str_lines.split(self.linesep)[0])

    def test_isatty(self):
        """Make sure we can check if the value is a tty"""
        # This should simply not fail
        self.assertTrue(self.spooled_flo.isatty() is True or
                        self.spooled_flo.isatty() is False)

    def test_truthy(self):
        """Make sure empty instances are still considered truthy"""
        self.spooled_flo.seek(0)
        self.spooled_flo.truncate()
        if not self.spooled_flo:
            raise AssertionError("Instance is not truthy")

    def test_instance_check(self):
        """Instance checks against IOBase succeed."""
        if not isinstance(self.spooled_flo, io.IOBase):
            raise AssertionError(f'{type(self.spooled_flo)} is not an instance of IOBase')

    def test_closed_file_method_valueerrors(self):
        """ValueError raised on closed files for certain methods."""
        self.spooled_flo.close()
        methods = (
            'flush', 'isatty', 'pos', 'buf', 'truncate', '__next__', '__iter__',
            '__enter__', 'read', 'readline', 'tell',
        )
        for method_name in methods:
            with self.assertRaises(ValueError):
                getattr(self.spooled_flo, method_name)()



class TestSpooledBytesIO(TestCase, BaseTestMixin, AssertionsMixin):
    linesep = os.linesep.encode('ascii')

    def setUp(self):
        self.spooled_flo = ioutils.SpooledBytesIO()
        self.test_str = b"Armado en los EE, UU. para S. P. Richards co.,"
        self.test_str_lines = (
            f"Text with:{os.linesep}newlines!".encode('ascii')
        )
        self.data_type = bytes

    def test_compare_not_equal_instances(self):
        """Make sure instances with different values fail == check."""
        a = ioutils.SpooledBytesIO()
        a.write(b"I am a!")
        b = ioutils.SpooledBytesIO()
        b.write(b"I am b!")
        self.assertNotEqual(a, b)

    def test_compare_two_equal_instances(self):
        """Make sure we can compare instances"""
        a = ioutils.SpooledBytesIO()
        a.write(b"I am equal!")
        b = ioutils.SpooledBytesIO()
        b.write(b"I am equal!")
        self.assertEqual(a, b)

    def test_auto_rollover(self):
        """Make sure file rolls over to disk after max_size reached"""
        tmp = ioutils.SpooledBytesIO(max_size=10)
        tmp.write(b"The quick brown fox jumped over the lazy dogs.")
        self.assertTrue(tmp._rolled)

    def test_use_as_context_mgr(self):
        """Make sure SpooledBytesIO can be used as a context manager"""
        test_str = b"Armado en los EE, UU. para S. P. Richards co.,"
        with ioutils.SpooledBytesIO() as f:
            f.write(test_str)
            self.assertEqual(f.getvalue(), test_str)

    def test_len_no_rollover(self):
        """Make sure len works with in-memory flo"""
        self.spooled_flo.write(self.test_str)
        self.assertEqual(self.spooled_flo.len, len(self.test_str))
        self.assertEqual(len(self.spooled_flo), len(self.test_str))

    def test_len_rollover(self):
        """Make sure len works with on-disk flo"""
        self.spooled_flo.write(self.test_str)
        self.spooled_flo.rollover()
        self.assertEqual(self.spooled_flo.len, len(self.test_str))
        self.assertEqual(len(self.spooled_flo), len(self.test_str))

    def test_invalid_type(self):
        """Ensure TypeError raised when writing unicode to SpooledBytesIO"""
        self.assertRaises(TypeError, self.spooled_flo.write, "hi")

    def test_flush_after_rollover(self):
        """Make sure we can flush before and after rolling to a real file"""
        self.spooled_flo.write(self.test_str)
        self.assertIsNone(self.spooled_flo.flush())
        self.spooled_flo.rollover()
        self.assertIsNone(self.spooled_flo.flush())

    def test_write_returns_byte_count(self):
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                with ioutils.SpooledBytesIO() as stream:
                    if rolled:
                        stream.rollover()
                    self.assertEqual(stream.write(b'abc'), 3)
                    self.assertEqual(stream.write(b''), 0)
                    self.assertEqual(stream.getvalue(), b'abc')

    def test_buffered_writer_flush(self):
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                stream = ioutils.SpooledBytesIO()
                if rolled:
                    stream.rollover()
                writer = io.BufferedWriter(stream)
                try:
                    writer.write(b'payload')
                    writer.flush()
                    self.assertEqual(stream.getvalue(), b'payload')
                finally:
                    try:
                        writer.close()
                    except BlockingIOError:
                        stream.close()

    def test_buffer_write_rollover_uses_byte_count(self):
        with ioutils.SpooledBytesIO(max_size=4) as stream:
            with memoryview(bytearray(b'abcd')).cast('I') as view:
                self.assertEqual(stream.write(view), 4)
            self.assertTrue(stream._rolled)
            self.assertEqual(stream.getvalue(), b'abcd')

    def test_zip_compat(self):
        """Make sure object is compatible with ZipFile library"""
        self.spooled_flo.seek(0)
        self.spooled_flo.truncate()
        doc = ZipFile(self.spooled_flo, 'w', ZIP_DEFLATED)
        doc.writestr("content.txt", "test")
        self.assertTrue('content.txt' in doc.namelist())
        doc.close()

    def test_iter(self):
        """Make sure iter works as expected"""
        self.spooled_flo.write(b"a\nb")
        self.spooled_flo.seek(0)
        self.assertEqual([x for x in self.spooled_flo], [b"a\n", b"b"])

    def test_writelines(self):
        """An iterable of lines can be written"""
        lines = [b"1", b"2", b"3"]
        expected = b"123"
        self.spooled_flo.writelines(lines)
        self.assertEqual(self.spooled_flo.getvalue(), expected)

    def test_readline_zero_length(self):
        self.spooled_flo.write(b"a\nb")
        self.spooled_flo.seek(0)
        self.assertEqual(self.spooled_flo.readline(0), b"")
        self.assertEqual(self.spooled_flo.readline(), b"a\n")


class TestSpooledStringIO(TestCase, BaseTestMixin, AssertionsMixin):
    linesep = os.linesep

    def setUp(self):
        self.spooled_flo = ioutils.SpooledStringIO()
        self.test_str = "Remember kids, always use an emdash: '\u2014'"
        self.test_str_lines = f"Text with\u2014{os.linesep}newlines!"
        self.data_type = str

    def test_compare_not_equal_instances(self):
        """Make sure instances with different values fail == check."""
        a = ioutils.SpooledStringIO()
        a.write("I am a!")
        b = ioutils.SpooledStringIO()
        b.write("I am b!")
        self.assertNotEqual(a, b)

    def test_readline_zero_length(self):
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = ioutils.SpooledStringIO()
                flo.write("a\nb")
                if rolled:
                    flo.rollover()
                flo.seek(0)
                self.assertEqual(flo.readline(0), "")
                self.assertEqual(flo.tell(), 0)
                self.assertEqual(flo.readline(), "a\n")

    def test_compare_two_equal_instances(self):
        """Make sure we can compare instances"""
        a = ioutils.SpooledStringIO()
        a.write("I am equal!")
        b = ioutils.SpooledStringIO()
        b.write("I am equal!")
        self.assertEqual(a, b)

    def test_auto_rollover(self):
        """Make sure file rolls over to disk after max_size reached"""
        tmp = ioutils.SpooledStringIO(max_size=10)
        tmp.write("The quick brown fox jumped over the lazy dogs.")
        self.assertTrue(tmp._rolled)

    def test_use_as_context_mgr(self):
        """Make sure SpooledStringIO can be used as a context manager"""
        test_str = "Armado en los EE, UU. para S. P. Richards co.,"
        with ioutils.SpooledStringIO() as f:
            f.write(test_str)
            self.assertEqual(f.getvalue(), test_str)

    def test_len_no_rollover(self):
        """Make sure len property works with in-memory flo"""
        self.spooled_flo.write(self.test_str)
        self.assertEqual(self.spooled_flo.len, len(self.test_str))

    def test_len_rollover(self):
        """Make sure len property works with on-disk flo"""
        self.spooled_flo.write(self.test_str)
        self.spooled_flo.rollover()
        self.assertEqual(self.spooled_flo.len, len(self.test_str))

    def test_invalid_type(self):
        """Ensure TypeError raised when writing bytes to SpooledStringIO"""
        self.assertRaises(TypeError, self.spooled_flo.write, b"hi")

    def test_write_returns_character_count(self):
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                with ioutils.SpooledStringIO() as stream:
                    if rolled:
                        stream.rollover()
                    self.assertEqual(stream.write('a\u2603b'), 3)
                    self.assertEqual(stream.write(''), 0)
                    self.assertEqual(stream.getvalue(), 'a\u2603b')

    def test_relative_backwards_seek_uses_character_position(self):
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                with ioutils.SpooledStringIO() as stream:
                    stream.write('a\u2603bc')
                    if rolled:
                        stream.rollover()
                    stream.seek(3)
                    self.assertEqual(stream.seek(-2, os.SEEK_CUR), 1)
                    self.assertEqual(stream.read(2), '\u2603b')
                    self.assertEqual(stream.tell(), 3)

    def test_negative_destination_rejected_without_moving(self):
        for rolled in (False, True):
            for offset, whence in ((-1, os.SEEK_SET), (-3, os.SEEK_CUR)):
                with self.subTest(rolled=rolled, offset=offset, whence=whence):
                    with ioutils.SpooledStringIO() as stream:
                        stream.write('a\u2603bc')
                        if rolled:
                            stream.rollover()
                        stream.seek(2)
                        with self.assertRaises(ValueError):
                            stream.seek(offset, whence)
                        self.assertEqual(stream.tell(), 2)
                        self.assertEqual(stream.read(), 'bc')

    def test_len_keeps_position(self):
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                with ioutils.SpooledStringIO() as stream:
                    stream.write('a\u2603bcd')
                    if rolled:
                        stream.rollover()
                    stream.seek(1)
                    self.assertEqual(stream.len, 5)
                    self.assertEqual(stream.tell(), 1)
                    self.assertEqual(stream.read(), '\u2603bcd')

    def test_seek_SEEK_END_negative_offset(self):
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                with ioutils.SpooledStringIO() as stream:
                    stream.write('a\u2603cde')
                    if rolled:
                        stream.rollover()
                    self.assertEqual(stream.seek(-2, os.SEEK_END), 3)
                    self.assertEqual(stream.read(), 'de')
                    with self.assertRaises(ValueError):
                        stream.seek(-6, os.SEEK_END)
                    self.assertEqual(stream.tell(), 5)

    def test_rollover_after_readline_keeps_position(self):
        with ioutils.SpooledStringIO() as stream:
            stream.write('fir\u2603t\nlast')
            stream.seek(0)
            self.assertEqual(stream.readline(), 'fir\u2603t\n')
            stream.rollover()
            self.assertEqual(stream.tell(), 6)
            self.assertEqual(stream.read(), 'last')

    def test_tell_codepoints(self):
        """Verify tell() returns codepoint position, not bytes position"""
        self.spooled_flo.write(self.test_str)
        self.spooled_flo.seek(0)
        self.spooled_flo.read(40)
        self.assertEqual(self.spooled_flo.tell(), 40)
        self.spooled_flo.seek(10)
        self.assertEqual(self.spooled_flo.tell(), 10)

    def test_codepoints_all_enc(self):
        """"Test getting read, seek, tell, on various codepoints"""
        test_str = "\u2014\u2014\u2014"
        self.spooled_flo.write(test_str)
        self.spooled_flo.seek(1)
        self.assertEqual(self.spooled_flo.read(), "\u2014\u2014")
        self.assertEqual(len(self.spooled_flo), len(test_str))

    def test_seek_codepoints_SEEK_END(self):
        """Make sure  seek() moves to codepoints relative to file end"""
        self.spooled_flo.write(self.test_str)
        ret = self.spooled_flo.seek(0, os.SEEK_END)
        self.assertEqual(ret, len(self.test_str))

    def test_seek_codepoints_large_SEEK_END(self):
        """Make sure seek() moves to codepoints relative to file end"""
        test_str = "".join(random.choice(string.ascii_letters) for
                            x in range(34000))
        self.spooled_flo.write(test_str)
        ret = self.spooled_flo.seek(0, os.SEEK_END)
        self.assertEqual(ret, len(test_str))

    def test_seek_codepoints_SEEK_SET(self):
        """Make sure seek() moves to codepoints relative to file start"""
        self.spooled_flo.write(self.test_str)
        ret = self.spooled_flo.seek(3, os.SEEK_SET)
        self.assertEqual(ret, 3)

    def test_seek_codepoints_large_SEEK_SET(self):
        """Make sure seek() moves to codepoints relative to file start"""
        test_str = "".join(random.choice(string.ascii_letters) for
                            x in range(34000))
        self.spooled_flo.write(test_str)
        ret = self.spooled_flo.seek(33000, os.SEEK_SET)
        self.assertEqual(ret, 33000)

    def test_seek_codepoints_SEEK_CUR(self):
        """Make sure seek() moves to codepoints relative to current_position"""
        test_str = "\u2014\u2014\u2014"
        self.spooled_flo.write(test_str)
        self.spooled_flo.seek(1)
        self.assertEqual(self.spooled_flo.tell(), 1)
        ret = self.spooled_flo.seek(2, os.SEEK_CUR)
        self.assertEqual(ret, 3)

    def test_seek_codepoints_large_SEEK_CUR(self):
        """Make sure seek() moves to codepoints relative to current_position"""
        test_str = "".join(random.choice(string.ascii_letters) for
                            x in range(34000))
        self.spooled_flo.write(test_str)
        self.spooled_flo.seek(1)
        ret = self.spooled_flo.seek(33000, os.SEEK_CUR)
        self.assertEqual(ret, 33001)

    def test_x80_codepoint(self):
        """Make sure x80 codepoint doesn't confuse read value"""
        test_str = '\x8000'
        self.spooled_flo.write(test_str)
        self.spooled_flo.seek(0)
        self.assertEqual(len(self.spooled_flo.read(2)), 2)
        self.assertEqual(self.spooled_flo.read(), '0')

    def test_seek_encoded(self):
        """Make sure reading works when bytes exceeds read val"""
        test_str = "\u2014\u2014\u2014"
        self.spooled_flo.write(test_str)
        self.spooled_flo.seek(0)
        self.assertEqual(self.spooled_flo.read(3), test_str)

    def test_iter(self):
        """Make sure iter works as expected"""
        self.spooled_flo.write("a\nb")
        self.spooled_flo.seek(0)
        self.assertEqual([x for x in self.spooled_flo], ["a\n", "b"])

    def test_writelines(self):
        """An iterable of lines can be written"""
        lines = ["1", "2", "3"]
        expected = "123"
        self.spooled_flo.writelines(lines)
        self.assertEqual(self.spooled_flo.getvalue(), expected)


class TestSpooledStringIOEncodedMode(TestCase):
    """Tests for SpooledStringIO's encoded-input write mode"""

    def make_flo(self, **kwargs):
        kwargs.setdefault('encoding', 'utf-8')
        return ioutils.SpooledStringIO(**kwargs)

    def test_encoding_property(self):
        self.assertEqual(self.make_flo().encoding, 'utf-8')
        self.assertIsNone(ioutils.SpooledStringIO().encoding)

    def test_chunked_multibyte_roundtrip(self):
        """Characters split across chunks are reassembled by the decoder"""
        text = 'a—b☃c'
        data = text.encode('utf-8')
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = self.make_flo()
                if rolled:
                    flo.rollover()
                total = 0
                for i in range(len(data)):
                    total += flo.write_encoded(data[i:i + 1])
                self.assertEqual(total, len(text))
                flo.finalize()
                self.assertEqual(flo.tell(), len(text))
                flo.seek(0)
                self.assertEqual(flo.read(), text)

    def test_write_encoded_returns_decoded_character_count(self):
        """Return value counts decoded characters, not input bytes"""
        emdash = '—'.encode('utf-8')
        self.assertEqual(len(emdash), 3)
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = self.make_flo()
                if rolled:
                    flo.rollover()
                self.assertEqual(flo.write_encoded(emdash[:1]), 0)
                self.assertEqual(flo.tell(), 0)
                self.assertEqual(flo.write_encoded(emdash[1:]), 1)
                self.assertEqual(flo.tell(), 1)
                self.assertEqual(flo.write_encoded(b''), 0)

    def test_write_encoded_accepts_bytes_like(self):
        flo = self.make_flo()
        self.assertEqual(flo.write_encoded(bytearray(b'ab')), 2)
        self.assertEqual(flo.write_encoded(memoryview(b'cd')), 2)
        flo.finalize()
        self.assertEqual(flo.getvalue(), 'abcd')

    def test_write_encoded_requires_encoding(self):
        """Encoded-input mode is fixed at construction time"""
        flo = ioutils.SpooledStringIO()
        self.assertRaises(TypeError, flo.write_encoded, b'abc')
        self.assertRaises(ValueError, flo.finalize)

    def test_write_encoded_type_checked(self):
        flo = self.make_flo()
        self.assertRaises(TypeError, flo.write_encoded, 'text')
        self.assertRaises(TypeError, flo.write_encoded, 42)

    def test_fixed_encoding_is_used(self):
        """A non-utf-8 input encoding decodes through the same interface"""
        text = 'héllo ☃'
        data = text.encode('utf-16-le')
        flo = self.make_flo(encoding='utf-16-le')
        flo.write_encoded(data[:3])
        flo.write_encoded(data[3:])
        flo.finalize()
        flo.seek(0)
        self.assertEqual(flo.read(), text)

    def test_pending_partial_character_blocks_text_write_seek_truncate(self):
        """Half a character pending: write/seek/truncate refuse clearly"""
        emdash = '—'.encode('utf-8')
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = self.make_flo()
                flo.write_encoded(b'ok')
                if rolled:
                    flo.rollover()
                flo.write_encoded(emdash[:1])
                with self.assertRaises(ValueError):
                    flo.write('x')
                with self.assertRaises(ValueError):
                    flo.seek(0)
                with self.assertRaises(ValueError):
                    flo.truncate()
                with self.assertRaises(ValueError):
                    flo.truncate(1)
                # completing the character performs a controlled unblock
                flo.write_encoded(emdash[1:])
                flo.write('!')
                flo.seek(0)
                self.assertEqual(flo.read(), 'ok—!')
                flo.truncate(2)
                self.assertEqual(flo.getvalue(), 'ok')

    def test_mixed_text_and_encoded_writes(self):
        """Plain text writes are fine whenever no character is pending"""
        flo = self.make_flo()
        flo.write('ab')
        flo.write_encoded('cd'.encode('utf-8'))
        flo.finalize()
        flo.write('ef')
        self.assertEqual(flo.tell(), 6)
        self.assertEqual(flo.getvalue(), 'abcdef')

    def test_finalize_detects_truncated_tail(self):
        """Only finalize() judges a truncated trailing character"""
        emdash = '—'.encode('utf-8')
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = self.make_flo()
                flo.write_encoded(b'abc')
                if rolled:
                    flo.rollover()
                flo.write_encoded(emdash[:2])
                # not yet judged: the caller may still complete the char
                self.assertEqual(flo.tell(), 3)
                with self.assertRaises(UnicodeDecodeError):
                    flo.finalize()
                # the incomplete character was never counted as written
                self.assertEqual(flo.tell(), 3)
                self.assertEqual(flo.getvalue(), 'abc')
                # pending state is discarded; the flo is usable again
                flo.seek(1)
                flo.truncate()
                self.assertEqual(flo.getvalue(), 'a')

    def test_finalize_idempotent_and_ends_encoded_input(self):
        flo = self.make_flo()
        flo.write_encoded('x☃'.encode('utf-8'))
        self.assertEqual(flo.finalize(), 0)
        self.assertEqual(flo.finalize(), 0)
        self.assertRaises(ValueError, flo.write_encoded, b'more')
        self.assertEqual(flo.getvalue(), 'x☃')

    def test_decode_error_counts_nothing_as_written(self):
        """Undecodable input raises without advancing the position"""
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = self.make_flo()
                flo.write_encoded(b'ab')
                if rolled:
                    flo.rollover()
                with self.assertRaises(UnicodeDecodeError):
                    flo.write_encoded(b'\xff')
                self.assertEqual(flo.tell(), 2)
                self.assertEqual(flo.len, 2)

    def test_rollover_failure_counts_nothing_as_written(self):
        """A failed rollover leaves position and contents untouched"""
        flo = self.make_flo(max_size=1)

        def _failing_rollover():
            raise OSError('disk full')

        flo.rollover = _failing_rollover
        with self.assertRaises(OSError):
            flo.write_encoded('—'.encode('utf-8'))
        self.assertEqual(flo.tell(), 0)
        self.assertFalse(flo._rolled)
        self.assertEqual(flo.getvalue(), '')

    def test_auto_rollover_with_split_characters(self):
        """Rollover works even while the decoder holds half a character"""
        text = '—' * 10
        data = text.encode('utf-8')
        flo = self.make_flo(max_size=10)
        for i in range(0, len(data), 2):
            flo.write_encoded(data[i:i + 2])
        flo.finalize()
        self.assertTrue(flo._rolled)
        self.assertEqual(flo.tell(), len(text))
        self.assertEqual(flo.len, len(text))
        self.assertEqual(flo.getvalue(), text)

    def test_character_positioning_contract(self):
        """tell/seek/read keep counting codepoints in encoded-input mode"""
        for rolled in (False, True):
            with self.subTest(rolled=rolled):
                flo = self.make_flo()
                flo.write_encoded('a☃bc'.encode('utf-8'))
                flo.finalize()
                if rolled:
                    flo.rollover()
                self.assertEqual(flo.tell(), 4)
                flo.seek(1)
                self.assertEqual(flo.read(1), '☃')
                self.assertEqual(flo.tell(), 2)
                self.assertEqual(flo.seek(-1, os.SEEK_END), 3)
                self.assertEqual(flo.read(), 'c')

    def test_closed_file_valueerrors(self):
        flo = self.make_flo()
        flo.close()
        self.assertRaises(ValueError, flo.write_encoded, b'a')
        self.assertRaises(ValueError, flo.finalize)


class TestMultiFileReader(TestCase):
    def test_read_seek_bytes(self):
        r = ioutils.MultiFileReader(io.BytesIO(b'narf'), io.BytesIO(b'troz'))
        self.assertEqual([b'nar', b'ftr', b'oz'],
                         list(iter(lambda: r.read(3), b'')))
        r.seek(0)
        self.assertEqual(b'narftroz', r.read())

    def test_read_seek_text(self):
        r = ioutils.MultiFileReader(io.StringIO('narf'),
                                    io.StringIO('troz'))
        self.assertEqual(['nar', 'ftr', 'oz'],
                         list(iter(lambda: r.read(3), '')))
        r.seek(0)
        self.assertEqual('narftroz', r.read())

    def test_read_zero_and_negative(self):
        r = ioutils.MultiFileReader(io.BytesIO(b'narf'), io.BytesIO(b'troz'))
        self.assertEqual(r.read(0), b'')
        self.assertEqual(r.read(5), b'narft')
        self.assertEqual(r.read(-1), b'roz')
        self.assertEqual(r.read(), b'')
        r.seek(0)
        self.assertEqual(r.read(3), b'nar')

    def test_no_mixed_bytes_and_text(self):
        self.assertRaises(ValueError, ioutils.MultiFileReader,
                          io.BytesIO(b'narf'), io.StringIO('troz'))

    def test_open(self):
        with open(CUR_FILE_PATH) as f:
            r_file_str = f.read()
        with open(CUR_FILE_PATH) as f1:
            with open(CUR_FILE_PATH) as f2:
                mfr = ioutils.MultiFileReader(f1, f2)
                r_double_file_str = mfr.read()

        assert r_double_file_str == (r_file_str * 2)

        with open(CUR_FILE_PATH, 'rb') as f:
            rb_file_str = f.read()
        with open(CUR_FILE_PATH, 'rb') as f1:
            with open(CUR_FILE_PATH, 'rb') as f2:
                mfr = ioutils.MultiFileReader(f1, f2)
                rb_double_file_str = mfr.read()

        assert rb_double_file_str == (rb_file_str * 2)

        utf8_file_str = codecs.open(CUR_FILE_PATH, encoding='utf8').read()
        f1, f2 = (codecs.open(CUR_FILE_PATH, encoding='utf8'),
                  codecs.open(CUR_FILE_PATH, encoding='utf8'))
        mfr = ioutils.MultiFileReader(f1, f2)
        utf8_double_file_str = mfr.read()
        assert utf8_double_file_str == (utf8_file_str * 2)
