import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars, SKIP, Val, GlomError, Path, PathAccessError

target = [[1, 2], [3, 'bad']]
spec = Iter(Iter(T.x))
gen = glom(target, spec)
try:
    for chunk in gen:
        print('chunk', list(chunk))
except Exception as e:
    print('ERR', type(e).__name__)
    print(str(e))
