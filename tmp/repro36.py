import sys
sys.path.insert(0, '.')
from glom import glom, Iter, GlomError

# top-level default cannot catch lazy errors (error raised during consumption,
# after glom returned) -- document current behavior is raw raise at consumption
def boom(t):
    raise ValueError('nope')

gen = glom([1], Iter(boom))
try:
    list(gen)
except GlomError as e:
    print('consumption raises finalized GlomError:', type(e).__name__)

# SKIP/STOP still work with new structure
from glom import SKIP, STOP
print(list(glom([1, SKIP, 2], Iter())))
print(list(glom([1, STOP, 2], Iter())))

# custom sentinel
print(list(glom([1, 99, 2], Iter(sentinel=99))))
