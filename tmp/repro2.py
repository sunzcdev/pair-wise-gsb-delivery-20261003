import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars, SKIP, Val

target = [[1, 2], [3, 4]]
spec2 = {'x': Iter().map((A.local, S.local)),
         'y': Iter().map((A.other, (S.other, S.local)))}
res2 = glom(target, spec2)
ix, iy = res2['x'], res2['y']
print('S leak:')
print(next(ix))   # expect (1, 1)
print(next(iy))   # expect (1, (1, ??? local not set in y -> error?))
print(next(ix))   # expect (2, 2)
print(next(iy))
