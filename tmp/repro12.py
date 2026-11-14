import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars

# flat target so S(v=T) binds scalar, and same key in both iters
target = [1, 2, 3]
spec = {'x': Iter().map({'w': (S(v=T), S.v)}),
        'y': Iter().map({'r': S.v})}
res = glom(target, spec)
ix, iy = res['x'], res['y']
print(next(ix))   # {'w': 1}, v=1 written into x Iter's scope map
try:
    print(next(iy))
except Exception as e:
    print('y error (isolated):', type(e).__name__)
