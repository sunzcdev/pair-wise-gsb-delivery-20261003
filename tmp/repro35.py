import sys
sys.path.insert(0, '.')
from glom import glom, Iter, T, S

# S binding inside map, read inside a later filter; interleaved two iters
target = [1, 2, 3, 4]
spec = {
    'evens': Iter().map((S(cur=T), T)).filter(lambda t: t % 2 == 0),
    'odds':  Iter().map((S(cur=T * 10), T)).filter(lambda t: t % 2 == 1),
}
res = glom(target, spec)
ie, io = res['evens'], res['odds']
print(next(ie), next(io), next(ie), next(io))
print(list(ie), list(io))
