class Foo:
    def m(self):
        return hasattr(self, '_Bar__x')
import inspect
print(inspect.getsource(Foo.m))
