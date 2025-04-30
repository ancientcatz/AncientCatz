#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<EOF
Usage: $(basename "$0") \
  -f OUTER_SVG -i INNER_SVG -x X -y Y -w W -h H -o OUTPUT_SVG [-m MARKER]
  -f  Outer SVG file (must contain marker)
  -i  Inner SVG file to inline
  -x  X position
  -y  Y position
  -w  Width of inline SVG
  -h  Height of inline SVG
  -o  Output SVG path
  -m  Marker string in outer SVG (default: <!-- INSERT_SVG_HERE -->)
EOF
  exit 1
}

# default marker
marker='<!-- INSERT_SVG_HERE -->'

# parse flags
while getopts "f:i:x:y:w:h:o:m:" opt; do
  case $opt in
    f) outer="$OPTARG";;
    i) inner="$OPTARG";;
    x) x="$OPTARG";;
    y) y="$OPTARG";;
    w) w="$OPTARG";;
    h) h="$OPTARG";;
    o) out="$OPTARG";;
    m) marker="$OPTARG";;
    *) usage;;
  esac
done

# ensure required vars
[[ -v outer && -v inner && -v x && -v y && -v w && -v h && -v out ]] || usage

echo "Embedding '$inner' into '$outer' → '$out' at ($x,$y) size ${w}×${h}"

# check xmlstarlet
if ! command -v xmlstarlet &>/dev/null; then
  echo "Error: xmlstarlet not found. Install via your package manager." >&2
  exit 1
fi

# ensure marker exists
if ! grep -Fq "$marker" "$outer"; then
  echo "Error: marker '$marker' not found in '$outer'." >&2
  exit 1
fi

# extract viewBox
viewBox=$(xmlstarlet sel \
  -N svg="http://www.w3.org/2000/svg" \
  -t -v "//svg:svg/@viewBox" \
  "$inner")
if [[ -z "$viewBox" ]]; then
  echo "Error: no viewBox on inner SVG." >&2
  exit 1
fi

# extract child nodes
innerContent=$(xmlstarlet sel \
  -N svg="http://www.w3.org/2000/svg" \
  -t -c "//svg:svg/*" \
  "$inner")
if [[ -z "$innerContent" ]]; then
  echo "Error: failed to extract content from inner SVG." >&2
  exit 1
fi

# prepare temp file for inline SVG
tmpblock=$(mktemp)
trap 'rm -f "$tmpblock"' EXIT

{
  printf '<svg x="%s" y="%s" width="%s" height="%s" viewBox="%s" xmlns="http://www.w3.org/2000/svg">\n' \
    "$x" "$y" "$w" "$h" "$viewBox"
  printf '%s\n' "$innerContent"
  printf '</svg>\n'
} > "$tmpblock"

# ensure output directory
mkdir -p "$(dirname "$out")"

# sed: at the line matching marker, read tmpblock (inserts its contents), then delete that marker line
sed "/$(printf '%s' "$marker" | sed 's/[^^]/[&]/g; s/\^/\\^/g')/ {
  r $tmpblock
  d
}" "$outer" > "$out"

echo "Done. Merged SVG written to '$out'."
