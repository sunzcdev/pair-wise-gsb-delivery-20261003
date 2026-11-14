import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A

# 1) same spec reused on different targets: no state inheritance
spec = Iter().map(T * 2)
r1 = list(glom([1, 2], spec))
r2 = list(glom([10, 20], spec))
print('reuse:', r1, r2)
assert r1 == [2, 4] and r2 == [20, 40]

# 2) shared object keeps identity, no deep copy
shared = {'k': 'v'}
target = [shared, shared]
out = list(glom(target, Iter()))
print('identity preserved:', out[0] is shared, out[1] is shared)
assert out[0] is shared and out[1] is shared

# 3) nested shared object identity
nested = {'items': [shared]}
out2 = list(glom([nested], Iter('items')))[0]
print('nested identity:', out2[0] is shared)
assert out2[0] is shared

# 4) scope-provided shared object identity
class Box: pass
box = Box()
out3 = list(glom([1, 2], Iter().map(S.box), scope={'box': box}))
print('scope object identity:', all(x is box for x in out3))
assert all(x is box for x in out3)
print('ALL OK')
