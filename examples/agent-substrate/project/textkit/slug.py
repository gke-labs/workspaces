# Copyright 2026 Google LLC
"""URL slugs."""

import re

def slugify(text):
    text = text.lower()
    text = re.sub(r"[^a-z0-9]+", "-", text)
    return text
