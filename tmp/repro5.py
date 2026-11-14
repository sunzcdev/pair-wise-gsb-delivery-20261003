import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars, SKIP, Val, GlomError, Path, PathAccessError

# compare eager list spec path behavior
target = [[1, 2], [3, 'bad']]
try:
    glom(target, [[T.x]])
except GlomError as e:
    print('EAGER:')
    print(str(e))
    print()

# lazy: check path inside scope via a raising function capturing scope
def boom(t):
    raise RuntimeError('boom')

spec = Iter(Iter(boom))
gen = glom(target, spec)
try:
    for chunk in gen:
        list(chunk)
except GlomError as e:
    print('LAZY:')
    print(str(e))
