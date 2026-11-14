import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, GlomError
from glom.core import Path as P

seen = []
class Fail:
    def glomit(self, target, scope):
        seen.append(list(scope[P]))
        raise KeyError('boom')

target = [[{'v': 1}], [{'v': 2}, {'oops': 3}]]
spec = Iter(Iter(Fail()))
gen = glom(target, spec)
try:
    for chunk in gen:
        list(chunk)
except GlomError as e:
    print('paths seen at failure:', seen)
    print('error scope path:', list(e._scope[P]))
    print()
    print(str(e))
