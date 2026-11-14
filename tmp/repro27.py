import sys
sys.path.insert(0, '.')
from glom.core import _DEFAULT_SCOPE, TargetRegistry, Path, MODE, MIN_MODE, CHILD_ERRORS, T, Spec, UP
from glom import glom, Iter
from xml.etree import ElementTree as ET

# simulate what Iter.glomit receives: find the scope passed to a top-level Iter glomit
captured = {}
class SpyIter(Iter):
    def glomit(self, target, scope):
        captured['scope'] = scope
        return super().glomit(target, scope)

el = ET.fromstring('<html><body/></html>')
try:
    list(glom(el, SpyIter()))
except Exception as e:
    print(type(e).__name__)
scope = captured['scope']
print('parent chain has registry:', any(TargetRegistry in m for m in scope.maps))
import glom.streaming as st
it_scope = st._iter_scope(scope)
print('iter_scope has registry:', any(TargetRegistry in m for m in it_scope.maps))
print('same registry:', it_scope[TargetRegistry] is _DEFAULT_SCOPE[TargetRegistry])
