# textkit: agent instructions

You are one of several agents, each assigned a single failing test module.

- Fix the bug in the library code under `textkit/` so that your assigned test
  module passes: `python3 -m unittest -q tests.test_<name>`.
- Do **not** modify anything under `tests/` or `tools/`, and do not touch
  modules unrelated to your bug.
- Keep the change minimal and keep the public function signatures unchanged.
- Only the Python standard library is available; do not install packages.
- When you are done, run the full suite (`python3 -m unittest discover -s tests -t .`)
  to make sure nothing else broke.
