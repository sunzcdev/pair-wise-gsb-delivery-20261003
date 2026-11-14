import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars, SKIP, GlomError, PathAccessError

target = {'a': [1, 2, 3], 'b': [10, 20, 30]}
spec = {'a': Iter().map(lambda x: ('a', x)), 'b': Iter().map(lambda x: ('b', x))}
res = glom(target, spec)
it_a = res['a']
it_b = res['b']
print('interleave:')
print(next(it_a))
print(next(it_b))
print(next(it_a))
print(next(it_b))
print(list(it_a))
print(list(it_b))
