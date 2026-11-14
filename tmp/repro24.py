import sys
sys.path.insert(0, '.')
from glom.core import _DEFAULT_SCOPE, TargetRegistry
reg = _DEFAULT_SCOPE[TargetRegistry]
from xml.etree import ElementTree as ET
el = ET.fromstring('<a/>')
print('default reg handler:', reg.get_handler('iterate', el, raise_exc=False))
print('cache:', list(reg._type_cache.items())[:3])
from glom import Glommer
g = Glommer()
print('glommer reg same?', g.scope[TargetRegistry] is reg)
print('glommer handler:', g.scope[TargetRegistry].get_handler('iterate', el, raise_exc=False))
