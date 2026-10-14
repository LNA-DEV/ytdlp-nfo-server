"""Regenerates the *.python.nfo golden files from the original ytdlp-nfo
implementation (create_nfo_from_json in ytdlp-nfo/main.py).

Usage: python3 -I gen_golden.py /path/to/ytdlp-nfo/main.py

Only the NFO functions are extracted, so yt_dlp does not need to be installed.
"""
import ast
import glob
import os
import sys

src = open(sys.argv[1], encoding="utf-8").read()
tree = ast.parse(src)
wanted = {"format_upload_date", "create_nfo_from_json"}
module = ast.Module(body=[n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name in wanted], type_ignores=[])
ns = {}
exec("import json\nfrom datetime import datetime\nimport xml.etree.ElementTree as ET\n", ns)
exec(compile(module, "main.py", "exec"), ns)

here = os.path.dirname(os.path.abspath(__file__))
for path in sorted(glob.glob(os.path.join(here, "*.info.json"))):
    out = path[: -len(".info.json")] + ".python.nfo"
    ns["create_nfo_from_json"](path, out)
    print("wrote", os.path.basename(out))
