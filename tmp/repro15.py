import sys, traceback
sys.path.insert(0, '.')
from glom import glom, Iter, T, GlomError

target = [[1, 2], [3, 'bad']]
spec = Iter(Iter(T.x))
gen = glom(target, spec)
try:
    for chunk in gen:
        list(chunk)
except GlomError as e:
    try:
        str(e)
    except Exception:
        traceback.print_exc()
