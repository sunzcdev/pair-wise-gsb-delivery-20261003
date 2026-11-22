# CI dependency locks for two runner targets

Offline, hash-pinned [PEP 751](https://peps.python.org/pep-0751/) lock files
that pre-provision dependencies for two CI runner classes. Everything is
served from the local `wheelhouse/`, so no index or network access is needed.

## Runner targets

| Lock | Runner | Declared target (`environments` / `requires-python`) |
| --- | --- | --- |
| `pylock.linux-py312.toml` | Linux x86_64, CPython 3.12 | `sys_platform == "linux" and python_version == "3.12"` |
| `pylock.windows-py313.toml` | Windows AMD64, CPython 3.13 | `sys_platform == "win32" and python_version == "3.13"` |
| `pylock.ci.toml` | both runners combined | both markers above, `requires-python >=3.12,<3.14` |
| `pylock.host-win-py312.toml` | native Windows/CPython 3.12 (real-pip e2e check on this machine) | `sys_platform == "win32" and python_version == "3.12"` |

## How locked artifacts map to targets

* **sharedlib 1.4.0** - a `py3-none-any` wheel selected identically on both
  runners (one shared artifact, one digest).
* **nativepkg 2.0** - the *same version number* resolves to two physically
  different wheels. Each keeps its own sha256 digest:
  - `...-cp312-cp312-manylinux_2_17_x86_64...whl` for the Linux runner,
  - `...-cp313-cp313-win_amd64.whl` for the Windows runner.
  Selection uses the wheel compatibility tags declared by each target.
* **pylib 1.0** - same version, interpreter-differentiated wheels
  (`cp312-none-any` vs `cp313-none-any`); the declared Python version picks
  the file.
* **osutil 1.0** - pure wheels whose platform is expressed through
  environment *markers*. The combined lock holds two package entries with
  mutually exclusive `sys_platform` markers, so each runner selects exactly
  one and no ambiguity is possible.
* **directref 3.1.0** - a direct file reference stored as a PEP 751
  `archive` entry (not an index lookup). Its provenance is retained as a
  relative `path = "wheelhouse/..."`, and pip reports it `is_direct = true`.

Every wheel/archive entry records the real sha256 of the file in
`wheelhouse/`, and relative paths are resolved against the lock file
location - the same base-directory rule pip applies to local locks.

## Reproducing

```sh
# 1. (Re)build the offline wheelhouse (stdlib only, no network).
python tools/build_wheelhouse.py

# 2. Generate the locks with pip's own vendored packaging.pylock model.
python tools/generate_locks.py

# 3. Verify each lock re-selects the same files for its declared targets.
python tools/verify_locks.py
```

`verify_locks.py` drives the exact `Pylock.select` machinery pip uses at
install time with simulated marker environments and tag sets, re-hashes each
selected file, and asserts the on-disk digest matches the lock. It also
checks that the single-target locks reject the other runner.

### Real-pip end-to-end check (on a Windows / CPython 3.12 host)

```sh
python -m pip install --no-index --dry-run -r pylock.host-win-py312.toml
python -m pip download --no-index -d downloaded -r pylock.host-win-py312.toml
```

The combined and single-target locks intentionally do not match this host,
so pip rejects them with an `environments` error - that is the target
guard working as designed. The native `pip lock` command only locks for the
running interpreter ("only guaranteed to be valid for the current python
version and platform"), which is why the multi-target combined lock is
assembled via the same underlying `packaging.pylock` API instead.
