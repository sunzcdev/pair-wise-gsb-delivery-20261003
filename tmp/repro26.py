import sys
sys.path.insert(0, '.')
from glom.core import _DEFAULT_SCOPE, TargetRegistry
from glom import glom, Iter, Fill, Ref, T
from xml.etree import ElementTree as ET

default_reg = _DEFAULT_SCOPE[TargetRegistry]
seen = {}
class Spy:
    def glomit(self, target, scope):
        reg = scope[TargetRegistry]
        seen['is_default'] = reg is default_reg
        seen['cache_before'] = dict(reg._type_cache)
        return target

el = ET.fromstring('<html><body/></html>')
spec = Fill(Ref('ET', (T.tag, (Iter(Spy()), list))))
try:
    glom(el, spec)
except Exception as e:
    print(type(e).__name__)
print('registry is default:', seen.get('is_default'))
