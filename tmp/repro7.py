import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, Inspect

paths = []
def inspector(target, spec, scope):
    from glom.core import Path as P
    paths.append(list(scope[P]))
    return target

insp = Inspect(recorder=lambda t: None) if False else None
# Inspect signature check
import inspect as _i
print(_i.signature(Inspect.__init__))
