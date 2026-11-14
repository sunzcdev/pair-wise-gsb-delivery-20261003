import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S, A, Vars

target = [[1, 2], [3, 4]]
spec = Iter(Iter().map({'assigned': (A.v, S.v), 'target': T}))
gen = glom(target, spec)
inner0 = next(gen)
inner1 = next(gen)
print(next(inner0))  # expect {'assigned':1,'target':1}
print(next(inner1))  # expect {'assigned':3,'target':3}
print(next(inner0))  # expect {'assigned':2,...}
print(next(inner1))  # expect {'assigned':4,...}
