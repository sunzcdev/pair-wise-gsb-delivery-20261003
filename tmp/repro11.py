import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars

# S(k=v) assignment does scope.update -> writes into current scope map (the Iter's captured scope!)
target = [[1, 2], [3, 4]]
spec = Iter(Iter().map((S(v=T), S.v)))
gen = glom(target, spec)
inner0 = next(gen)
inner1 = next(gen)
print('interleaved S(v=T):')
print(next(inner0))  # expect 1
print(next(inner1))  # expect 3
print(next(inner0))  # expect 2
print(next(inner1))  # expect 4
