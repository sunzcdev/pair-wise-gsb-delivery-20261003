# Test-vector generators

The JSON files in the parent directory are byte-level expected outputs for
the Exclusive XML Canonicalization tests. They were generated with
[lxml](https://lxml.de/) (`method="c14n", exclusive=True`), whose
implementation passes the W3C Exclusive XML Canonicalization test suite:

- `genvectors.py` produces `../c14n_vectors.json`: hand-selected edge cases
  plus the W3C Recommendation Appendix B example document and its
  `InclusiveNamespaces PrefixList` variant.
- `genw3c.py` produces `../c14n_w3c.json`: canonical W3C-style visibility,
  attribute-ordering, and default-namespace vectors.
- `genfuzz.py` produces `../c14n_fuzz.json`: randomized namespace-heavy
  documents and subtrees (run with Python 3 and lxml installed).

Regenerate with:

```sh
cd testdata/gen
python genvectors.py
python genw3c.py
python genfuzz.py
```
