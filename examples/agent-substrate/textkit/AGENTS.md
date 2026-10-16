# textkit: agent instructions

`textkit` is a tiny, dependency-free Python text utility library.

- Library code lives in `textkit/`; tests live in `tests/` (one module per feature).
- Run one test module: `python3 -m unittest -q tests.test_<name>`.
- Run the whole suite: `python3 -m unittest discover -s tests -t .`
- Fix bugs in the library code, never by editing the tests.
- Keep changes minimal and keep public function signatures unchanged.
- Only the Python standard library is available; do not install packages.
