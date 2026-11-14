import sys
sys.path.insert(0, '.')
from glom.core import _DEFAULT_SCOPE, TargetRegistry
from glom import glom, Fill, Ref, T, Iter
from xml.etree import ElementTree as ET
el = ET.fromstring('<html><body/></html>')
spec = Fill(Ref('ET', (T.tag, (Iter(Ref('ET')), list))))
print(glom(el, spec))
reg = _DEFAULT_SCOPE[TargetRegistry]
print('iterate type_map keys:', list(reg._op_type_map['iterate'].keys()))
print('Element handler:', reg._op_type_map['iterate'].get(type(el)))
print('cache:', reg._type_cache)
