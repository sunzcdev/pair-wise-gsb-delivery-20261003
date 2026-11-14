import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, GlomError

target = [[1, 2], [3, 'bad']]
spec = Iter(Iter(T.x))
gen = glom(target, spec)
try:
    for chunk in gen:
        list(chunk)
except GlomError as e:
    print('type:', type(e).__name__)
    print('has _scope:', hasattr(e, '_scope'))
    print('repr:', repr(e))
    try:
        print('str:', str(e))
    except Exception as ex:
        print('STR FAILS:', type(ex).__name__, ex)
