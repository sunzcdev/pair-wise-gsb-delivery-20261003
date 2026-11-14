import sys
sys.path.insert(0, '.')
from xml.etree import ElementTree as ET
from glom import glom, Iter, Fill, Ref, T

etree = ET.fromstring('<html><body><p>x</p></body></html>')
spec = Fill(Ref('ET', (T.tag, (Iter(Ref('ET')), list))))
try:
    print(glom(etree, spec))
except Exception as e:
    print(type(e).__name__, e)
