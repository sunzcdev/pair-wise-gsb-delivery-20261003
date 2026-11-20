# Fixed-runner seed manifest demo

End-to-end demonstration of `--seed-manifest` / `--strict-seed-manifest` on a Windows runner.

## Fixtures

`build_demo.py` creates `wheelhouse/` with **two local candidates of the same pip version**:

| candidate | file name | distribution/version | sha256 |
| --- | --- | --- | --- |
| genuine | `pip-26.3-py3-none-any.whl` | matches the manifest | matches the manifest |
| tampered | `pip-26.3-py2.py3-none-any.whl` | matches the manifest | does **not** match |

Both wheels carry valid `Requires-Python` metadata for the running interpreter, so the tampered
candidate passes the name/version/interpreter checks and only loses on the declared digest.

The manifests:

- `seed-manifest.json` - strict, pins the genuine wheel (version + digest + file name)
- `seed-manifest-bad-digest.json` - strict, digest that neither candidate matches (no file-name pin)
- `seed-manifest-loose.json` - non-strict, digest that neither candidate matches

## Run

```powershell
.\.venv\Scripts\python.exe .demo\build_demo.py   # (re)build the wheelhouse and manifests
.\.venv\Scripts\python.exe .demo\run_demo.py     # run the four scenarios
```

Scenarios:

1. strict manifest satisfied by the genuine wheel -> that exact wheel is seeded, no tamper marker
2. strict manifest with only a version-matching candidate -> `SeedManifestError`, exit code 1
3. loose manifest digest mismatch -> warning, then the regular bundled selection still seeds
4. no manifest -> ordinary virtualenv selection is unchanged
