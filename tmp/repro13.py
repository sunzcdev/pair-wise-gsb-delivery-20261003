import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T

# side-effect counting source
class CountingSource:
    def __init__(self, n):
        self.n = n
        self.pulled = 0
    def __iter__(self):
        return self
    def __next__(self):
        if self.pulled >= self.n:
            raise StopIteration
        v = self.pulled
        self.pulled += 1
        return v

calls = []
def mapping(x):
    calls.append(x)
    return x * 10

# building result must not call mapping
src = CountingSource(5)
result = glom(src, Iter().map(mapping))
print('pulled after build:', src.pulled, 'mapping calls:', len(calls))
it = iter(result)
print('after iter():', src.pulled, len(calls))
print('next:', next(it), src.pulled, len(calls))
rest = list(it)
print('rest:', rest, 'pulled:', src.pulled, 'calls:', len(calls))

# limit boundary
src2 = CountingSource(100)
r2 = glom(src2, Iter().map(mapping).limit(3))
out = list(r2)
print('limited:', out, 'source pulled:', src2.pulled)
