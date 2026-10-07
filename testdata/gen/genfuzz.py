import io, json, random, subprocess
from lxml import etree

random.seed(1234)

PREFIXES = ["a", "b", "c", "n1", "n2", "z"]

def gen_decl(indent_depth):
    # choose declarations to place on an element
    decls = []
    if random.random() < 0.5:
        decls.append(('default', random.choice(["urn:def", "urn:other", ""])))
    for p in random.sample(PREFIXES, random.randint(0, 2)):
        decls.append((p, random.choice(["urn:"+p, "urn:shared", "urn:"+p+"-v2"])))
    return decls

def gen_element(depth, maxdepth, inscope):
    # inscope: dict prefix -> uri ("" = default)
    tag = []
    available = [None]  # no prefix
    for p, u in inscope.items():
        if p == "default":
            continue
        if u != "":
            available.append(p)
    pfx = random.choice(available)
    name = ("%s:e%d" % (pfx, depth)) if pfx else "e%d" % depth
    tag.append(name)

    # declarations on this element
    decls = gen_decl(depth)
    newscope = dict(inscope)
    for p, u in decls:
        newscope[p] = u

    attrs = []
    # namespace attrs written via etree serialization: we build raw xml string instead
    return name, newscope, decls

def build_xml(depth, maxdepth, inscope, pieces, path, paths):
    name, newscope, decls = gen_element(depth, maxdepth, inscope)
    paths.append((name, ".".join(str(x) for x in path)))
    pieces.append("<" + name)
    for p, u in decls:
        if p == "default":
            pieces.append(' xmlns="%s"' % u)
        else:
            pieces.append(' xmlns:%s="%s"' % (p, u))
    # attributes using visible prefixes
    used_prefixes = [p for p, u in newscope.items() if p != "default" and u != ""]
    for p in random.sample(used_prefixes, min(len(used_prefixes), random.randint(0, 2))):
        pieces.append(' %s:attr%d="v%d"' % (p, depth, depth))
    # unnamespaced attr
    if random.random() < 0.4:
        pieces.append(' plain="%d"' % depth)
    # xml attr
    if random.random() < 0.2:
        pieces.append(' xml:lang="en"')
    pieces.append(">")

    if depth < maxdepth:
        nchildren = random.randint(0, 3)
        for i in range(nchildren):
            r = random.random()
            if r < 0.15:
                pieces.append("<!--c%d-->" % depth)
            elif r < 0.25:
                pieces.append("<?pi%d data?>" % depth)
            elif r < 0.35:
                pieces.append("text%d " % depth)
            else:
                build_xml(depth+1, maxdepth, newscope, pieces, path+[element_index(pieces)], paths)
    pieces.append("</%s>" % name)

def element_index(pieces):
    return 0

cases = []
for i in range(300):
    pieces = []
    paths = []
    build_xml(0, random.randint(1, 4), {}, pieces, [], paths)
    xml = "".join(pieces)
    # validate it parses
    try:
        doc = etree.parse(io.BytesIO(xml.encode()))
    except Exception:
        continue
    # choose subtree paths (element children indices) - derive via tree walk
    elem_paths = []
    def walk(node, prefix):
        ei = 0
        for ch in node:
            if isinstance(ch.tag, str):
                p = prefix + [ei]
                elem_paths.append(p)
                walk(ch, p)
                ei += 1
    walk(doc.getroot(), [])
    subtree = random.choice(elem_paths) if elem_paths and random.random() < 0.5 else None
    incl = []
    if random.random() < 0.4:
        incl = random.sample(PREFIXES + ["#default"], random.randint(1, 2))
    wc = random.random() < 0.5
    try:
        if subtree is not None:
            node = doc.getroot()
            for idx in subtree:
                count = idx
                for ch in node:
                    if isinstance(ch.tag, str):
                        if count == 0:
                            node = ch
                            break
                        count -= 1
            exp = etree.tostring(node, method="c14n", exclusive=True, with_comments=wc,
                                 inclusive_ns_prefixes=incl or None).decode()
            mode = "subtree"
            spath = ".".join(str(x) for x in subtree)
        else:
            exp = etree.tostring(doc, method="c14n", exclusive=True, with_comments=wc,
                                 inclusive_ns_prefixes=incl or None).decode()
            mode = "doc"
            spath = ""
    except Exception:
        continue
    cases.append(dict(name="fuzz-%d" % i, xml=xml, mode=mode, with_comments=wc,
                      inclusive=incl, expected=exp, path=spath))

with io.open("../c14n_fuzz.json", "w", encoding="utf-8") as f:
    json.dump(cases, f, ensure_ascii=False)
print("generated", len(cases))
