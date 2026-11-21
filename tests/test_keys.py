import unittest

import cachetools.keys


class CacheKeysTest(unittest.TestCase):
    def test_hashkey(self, key=cachetools.keys.hashkey):
        self.assertEqual(key(), key())
        self.assertEqual(hash(key()), hash(key()))
        self.assertEqual(key(1, 2, 3), key(1, 2, 3))
        self.assertEqual(hash(key(1, 2, 3)), hash(key(1, 2, 3)))
        self.assertEqual(key(1, 2, 3, x=0), key(1, 2, 3, x=0))
        self.assertEqual(hash(key(1, 2, 3, x=0)), hash(key(1, 2, 3, x=0)))
        self.assertNotEqual(key(1, 2, 3), key(3, 2, 1))
        self.assertNotEqual(key(1, 2, 3), key(1, 2, 3, x=None))
        self.assertNotEqual(key(1, 2, 3, x=0), key(1, 2, 3, x=None))
        self.assertNotEqual(key(1, 2, 3, x=0), key(1, 2, 3, y=0))
        with self.assertRaises(TypeError):
            hash(key({}))
        # untyped keys compare equal
        self.assertEqual(key(1, 2, 3), key(1.0, 2.0, 3.0))
        self.assertEqual(hash(key(1, 2, 3)), hash(key(1.0, 2.0, 3.0)))

    def test_methodkey(self, key=cachetools.keys.methodkey):
        # similar to hashkey(), but ignores its first positional argument
        self.assertEqual(key("x"), key("y"))
        self.assertEqual(hash(key("x")), hash(key("y")))
        self.assertEqual(key("x", 1, 2, 3), key("y", 1, 2, 3))
        self.assertEqual(hash(key("x", 1, 2, 3)), hash(key("y", 1, 2, 3)))
        self.assertEqual(key("x", 1, 2, 3, x=0), key("y", 1, 2, 3, x=0))
        self.assertEqual(hash(key("x", 1, 2, 3, x=0)), hash(key("y", 1, 2, 3, x=0)))
        self.assertNotEqual(key("x", 1, 2, 3), key("x", 3, 2, 1))
        self.assertNotEqual(key("x", 1, 2, 3), key("x", 1, 2, 3, x=None))
        self.assertNotEqual(key("x", 1, 2, 3, x=0), key("x", 1, 2, 3, x=None))
        self.assertNotEqual(key("x", 1, 2, 3, x=0), key("x", 1, 2, 3, y=0))
        with self.assertRaises(TypeError):
            hash(key("x", {}))
        # untyped keys compare equal
        self.assertEqual(key("x", 1, 2, 3), key("y", 1.0, 2.0, 3.0))
        self.assertEqual(hash(key("x", 1, 2, 3)), hash(key("y", 1.0, 2.0, 3.0)))
        # "self" is positional-only, so it may also be a keyword argument
        self.assertEqual(key("x", self=1), key("y", self=1))
        self.assertNotEqual(key("x", self=1), key("x"))

    def test_typedkey(self, key=cachetools.keys.typedkey):
        self.assertEqual(key(), key())
        self.assertEqual(hash(key()), hash(key()))
        self.assertEqual(key(1, 2, 3), key(1, 2, 3))
        self.assertEqual(hash(key(1, 2, 3)), hash(key(1, 2, 3)))
        self.assertEqual(key(1, 2, 3, x=0), key(1, 2, 3, x=0))
        self.assertEqual(hash(key(1, 2, 3, x=0)), hash(key(1, 2, 3, x=0)))
        self.assertNotEqual(key(1, 2, 3), key(3, 2, 1))
        self.assertNotEqual(key(1, 2, 3), key(1, 2, 3, x=None))
        self.assertNotEqual(key(1, 2, 3, x=0), key(1, 2, 3, x=None))
        self.assertNotEqual(key(1, 2, 3, x=0), key(1, 2, 3, y=0))
        with self.assertRaises(TypeError):
            hash(key({}))
        # typed keys compare unequal
        self.assertNotEqual(key(1, 2, 3), key(1.0, 2.0, 3.0))
        self.assertNotEqual(key(1, 2, 3, x=0), key(1, 2, 3, x=0.0))
        self.assertNotEqual(key(1, 2, 3, x=0), key(1, 2, 3, x=False))

    def test_typedmethodkey(self, key=cachetools.keys.typedmethodkey):
        # similar to typedkey(), but ignores its first positional argument
        self.assertEqual(key("x"), key("y"))
        self.assertEqual(hash(key("x")), hash(key("y")))
        self.assertEqual(key("x", 1, 2, 3), key("y", 1, 2, 3))
        self.assertEqual(hash(key("x", 1, 2, 3)), hash(key("y", 1, 2, 3)))
        self.assertEqual(key("x", 1, 2, 3, x=0), key("y", 1, 2, 3, x=0))
        self.assertEqual(hash(key("x", 1, 2, 3, x=0)), hash(key("y", 1, 2, 3, x=0)))
        self.assertNotEqual(key("x", 1, 2, 3), key("x", 3, 2, 1))
        self.assertNotEqual(key("x", 1, 2, 3), key("x", 1, 2, 3, x=None))
        self.assertNotEqual(key("x", 1, 2, 3, x=0), key("x", 1, 2, 3, x=None))
        self.assertNotEqual(key("x", 1, 2, 3, x=0), key("x", 1, 2, 3, y=0))
        with self.assertRaises(TypeError):
            hash(key("x", {}))
        # typed keys compare unequal
        self.assertNotEqual(key("x", 1, 2, 3), key("x", 1.0, 2.0, 3.0))
        self.assertNotEqual(key("x", 1, 2, 3, x=0), key("x", 1, 2, 3, x=0.0))
        self.assertNotEqual(key("x", 1, 2, 3, x=0), key("x", 1, 2, 3, x=False))
        # "self" is positional-only, so it may also be a keyword argument
        self.assertEqual(key("x", self=1), key("y", self=1))
        self.assertNotEqual(key("x", self=1), key("x", self=1.0))

    def test_addkeys(self, key=cachetools.keys.hashkey):
        self.assertIsInstance(key(), tuple)
        self.assertIsInstance(key(1, 2, 3) + key(4, 5, 6), type(key()))
        self.assertIsInstance(key(1, 2, 3) + (4, 5, 6), type(key()))
        self.assertIsInstance((1, 2, 3) + key(4, 5, 6), type(key()))

    def test_pickle(self, key=cachetools.keys.hashkey):
        import pickle

        for k in [key(), key("abc"), key("abc", 123), key("abc", q="abc")]:
            h = hash(k)
            self.assertEqual(h, hash(tuple(k)))
            pickled = pickle.loads(pickle.dumps(k))
            self.assertEqual(k, pickled)
            self.assertEqual(h, hash(pickled))


class BoundKeyTest(unittest.TestCase):
    class Example:
        def method(self, a, b=2, *, c=3):
            return a, b, c

        def variadic(self, *args, **kwargs):
            return args, kwargs

    def setUp(self):
        self.key = cachetools.keys.boundkey(
            self.Example.method, "BoundKeyTest.Example.method"
        )
        self.first = self.Example()
        self.second = self.Example()

    def test_equivalent_calls_share_a_key(self):
        # explicit defaults, the default omitted and positional vs. keyword
        self.assertEqual(self.key(self.first, 1), self.key(self.first, 1, 2, c=3))
        self.assertEqual(self.key(self.first, 1), self.key(self.first, a=1))
        self.assertEqual(self.key(self.first, 1, 2), self.key(self.first, 1, b=2))

    def test_receiver_is_ignored(self):
        self.assertEqual(self.key(self.first, 1), self.key(self.second, 1))
        # the receiver never appears among the normalized items
        items = self.key(self.first, 1)[2]
        self.assertEqual([name for name, _ in items], ["a", "b", "c"])

    def test_namespace_is_part_of_the_key(self):
        other = cachetools.keys.boundkey(self.Example.method, "other.namespace")
        self.assertNotEqual(self.key(self.first, 1), other(self.first, 1))

    def test_distinct_arguments_stay_distinct(self):
        self.assertNotEqual(self.key(self.first, 1), self.key(self.first, 2))
        self.assertNotEqual(self.key(self.first, 1, c=3), self.key(self.first, 1, c=4))

    def test_variadic_arguments(self):
        key = cachetools.keys.boundkey(
            self.Example.variadic, "BoundKeyTest.Example.variadic"
        )
        self.assertEqual(
            key(self.first, 1, 2, z=3, y=4),
            key(self.first, 1, 2, y=4, z=3),
        )
        self.assertNotEqual(key(self.first, 1), key(self.first, 1, 2))

    def test_invalid_calls_raise_type_error(self):
        with self.assertRaises(TypeError):
            self.key(self.first)  # missing required argument
        with self.assertRaises(TypeError):
            self.key(self.first, 1, 2, 3, 4)  # too many positional arguments

    def test_key_is_a_plain_tuple(self):
        key = self.key(self.first, 1)
        self.assertIsInstance(key, tuple)
        hash(key)  # must be usable as a cache mapping key
