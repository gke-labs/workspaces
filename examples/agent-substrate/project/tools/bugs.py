# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""The bug catalogue shared by the repair bot, the report and the notebook.

Each entry maps a bug id to the module that contains the bug and the unittest
module that exposes it. One AX Task is dispatched per bug.
"""

BUGS = {
    "chunk": ("textkit/chunking.py", "tests.test_chunk"),
    "roman": ("textkit/roman.py", "tests.test_roman"),
    "median": ("textkit/stats.py", "tests.test_median"),
    "leap-year": ("textkit/dates.py", "tests.test_leap_year"),
    "slugify": ("textkit/slug.py", "tests.test_slugify"),
    "palindrome": ("textkit/palindrome.py", "tests.test_palindrome"),
}
