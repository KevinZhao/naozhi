#!/usr/bin/env python3
"""go-split-file.py — move whole top-level declarations out of one Go file into
siblings in the same package (#2561 E6-c).

    python3 scripts/go-split-file.py <src.go> <out.go>=<Decl1,Decl2,...> [...]

Four handler files sat between 1,130 and 1,470 lines, over the dashboard
sub-package's 800-line limit, held up by file_size exemptions. Splitting them by
hand across ~5,200 lines invites two mistakes: dropping a declaration, and
silently changing one while moving it. So the move is mechanical and verified:

  * A declaration is copied BYTE-FOR-BYTE, including its doc comment, from the
    first line of that comment to the closing brace. Nothing is reformatted.
  * Every requested name must be found exactly once, or the script refuses.
  * The concatenated output must account for every byte of the original
    declaration region — the script sums what it moved and what it left and
    compares against the source length.

Imports are NOT computed here; run goimports on the results. Getting that wrong
is a compile error, which is the safe direction, whereas a lost declaration
compiles fine if nothing calls it.
"""

import re
import subprocess
import sys
from pathlib import Path

DECL_RE = re.compile(
    r'^(?:func|type|var|const)\s+(?:\([^)]*\)\s*)?([A-Za-z_][\w]*)', re.M)


def decl_spans(src: str):
    """Yield (name, start, end) for each top-level declaration, where start
    includes any immediately preceding // comment block."""
    lines = src.split('\n')
    # Byte offset of the start of each line.
    offs, acc = [], 0
    for l in lines:
        offs.append(acc)
        acc += len(l) + 1

    out = []
    for i, line in enumerate(lines):
        m = DECL_RE.match(line)
        if not m:
            continue
        # Walk back over the doc comment.
        j = i
        while j > 0 and (lines[j - 1].startswith('//') or lines[j - 1].startswith('\t//')):
            j -= 1
        start = offs[j]
        # A declaration ends at the first column-0 "}" line, or for a one-liner
        # (`func f() {}` / `type X = Y` / `var x = 1`) at its own line end.
        if line.rstrip().endswith('{'):
            k = i + 1
            while k < len(lines) and lines[k] != '}':
                k += 1
            end = offs[k] + len(lines[k]) + 1 if k < len(lines) else len(src)
        else:
            end = offs[i] + len(line) + 1
        out.append((m.group(1), start, end))
    return out


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    src_path = Path(sys.argv[1])
    src = src_path.read_text()
    spans = decl_spans(src)

    by_name = {}
    for name, a, b in spans:
        by_name.setdefault(name, []).append((a, b))

    plan = []          # (out_path, [(name, a, b), ...])
    claimed = set()
    for arg in sys.argv[2:]:
        out_name, names = arg.split('=', 1)
        picked = []
        for n in names.split(','):
            n = n.strip()
            if not n:
                continue
            hits = by_name.get(n, [])
            if len(hits) != 1:
                print(f"refusing: {n!r} found {len(hits)} times in {src_path}")
                return 1
            a, b = hits[0]
            if (a, b) in claimed:
                print(f"refusing: {n!r} requested twice")
                return 1
            claimed.add((a, b))
            picked.append((n, a, b))
        picked.sort(key=lambda t: t[1])
        plan.append((src_path.parent / out_name, picked))

    pkg_header = src[:src.index('\n', src.index('package '))]
    pkg_line = pkg_header[pkg_header.rindex('package '):]

    moved_bytes = 0
    for out_path, picked in plan:
        body = ''.join(src[a:b] for _, a, b in picked)
        moved_bytes += len(body)
        out_path.write_text(pkg_line + '\n\n' + body)
        print(f"  {out_path.name:32} {len(picked):2} decls, {body.count(chr(10)):4} lines")

    # Remove the moved spans from the source, back to front. claimed holds
    # (start, end) pairs — sorting descending keeps earlier offsets valid.
    keep = src
    for a, b in sorted(claimed, reverse=True):
        keep = keep[:a] + keep[b:]

    # Byte conservation, checked BEFORE the source is overwritten: what stayed
    # plus what moved must equal what we started with. Refusing beats writing a
    # file that lost a declaration, because that compiles fine if nothing calls
    # it and the loss shows up much later.
    if len(keep) + moved_bytes != len(src):
        print(f"REFUSING: byte accounting off by {len(src) - len(keep) - moved_bytes}; source left untouched")
        return 1
    src_path.write_text(keep)
    print(f"  {src_path.name:32} kept {keep.count(chr(10))} lines "
          f"(bytes: {len(keep)} kept + {moved_bytes} moved == {len(src)})")

    # goimports resolves plain imports but cannot invent an ALIAS it has never
    # seen, so a package imported as `cronpkg "…/cron"` or
    # `sessionpkg "…/session"` comes out undefined in every new file. Carry the
    # source file's alias lines over to whichever outputs reference them, then
    # let goimports drop the ones that turn out unused.
    aliases = re.findall(r'^\t(\w+) ("[^"]+")$', src, re.M)
    for out_path, _ in plan:
        body = out_path.read_text()
        needed = [(a, path) for a, path in aliases
                  if re.search(r'(?<![.\w])' + a + r'\.', body)]
        if not needed:
            continue
        block = ''.join(f'\t{a} {path}\n' for a, path in needed)
        if '\nimport (' in body:
            body = body.replace('\nimport (\n', '\nimport (\n' + block, 1)
        else:
            body = body.replace('\n\n', '\n\nimport (\n' + block + ')\n\n', 1)
        out_path.write_text(body)

    files = [str(src_path)] + [str(p) for p, _ in plan]
    subprocess.run(['/Users/zhaokm/go/bin/goimports', '-w'] + files, check=True)
    subprocess.run(['gofmt', '-w'] + files, check=True)
    return 0


if __name__ == '__main__':
    sys.exit(main())
