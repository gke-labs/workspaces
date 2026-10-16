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

import unittest

from textkit.chunking import chunk


class ChunkTest(unittest.TestCase):
    def test_uneven(self):
        self.assertEqual(chunk([1, 2, 3, 4, 5], 2), [[1, 2], [3, 4], [5]])

    def test_even(self):
        self.assertEqual(chunk("abcdef", 3), [["a", "b", "c"], ["d", "e", "f"]])

    def test_size_one(self):
        self.assertEqual(chunk([1, 2, 3], 1), [[1], [2], [3]])

    def test_empty(self):
        self.assertEqual(chunk([], 4), [])

    def test_bad_size(self):
        with self.assertRaises(ValueError):
            chunk([1], 0)
