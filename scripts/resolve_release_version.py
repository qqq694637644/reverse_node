#!/usr/bin/env python3
"""Resolve a reverse_node GitHub Release tag.

An explicit version wins. Otherwise, find the highest stable published release
and increment only its patch component. An empty release list starts at 0.0.1.
The automatic format is X.Y.Z without a leading "v".
"""

import argparse
import json
import re
import sys
from pathlib import Path

VERSION_RE = re.compile(r"^(?:v)?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")


def parse_version(tag: str):
    match = VERSION_RE.fullmatch(tag)
    if match is None:
        return None
    return tuple(int(number) for number in match.groups())


def next_release_version(releases: list, requested: str = "") -> str:
    requested = requested.strip()
    if requested:
        if parse_version(requested) is None:
            raise ValueError("Version must be X.Y.Z or vX.Y.Z, e.g. 0.0.2")
        return requested

    versions = []
    for release in releases:
        if not isinstance(release, dict) or release.get("isDraft") or release.get("isPrerelease"):
            continue
        version = parse_version(str(release.get("tagName", "")))
        if version is not None:
            versions.append(version)

    if not versions:
        return "0.0.1"

    major, minor, patch = max(versions)
    return f"{major}.{minor}.{patch + 1}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--releases", required=True, type=Path, help="JSON array from gh release list")
    parser.add_argument("--version", default="", help="Optional explicit tag; empty means increment patch")
    arguments = parser.parse_args()

    try:
        releases = json.loads(arguments.releases.read_text(encoding="utf-8"))
        if not isinstance(releases, list):
            raise ValueError("Release listing must be a JSON array")
        print(next_release_version(releases, arguments.version))
    except (OSError, json.JSONDecodeError, ValueError) as error:
        print(f"Cannot resolve release version: {error}", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
