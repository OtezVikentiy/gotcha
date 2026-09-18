#!/usr/bin/env bash
# Собирает PDF-комплект bare-metal документации (5 страниц, ru и en) через pandoc/xelatex.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: docs-pdf.sh --version X.Y.Z [--out DIR]

Builds two PDFs (ru, en) from the bare-metal documentation bundle
(installation-bare-metal, configuration, hardening, backup-restore, upgrade):
DIR/gotcha-bare-metal-<locale>-X.Y.Z.pdf.

Requires on PATH: pandoc, a xelatex from a TeX Live install with the fvextra
package, pdftotext (poppler-utils), python3, and the DejaVu font family.
EOF
}

VERSION=""
OUT=""

while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            VERSION="${2:-}"
            shift 2
            ;;
        --out)
            OUT="${2:-}"
            shift 2
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            echo "docs-pdf.sh: unknown argument: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

[ -n "$VERSION" ] || { echo "docs-pdf.sh: --version is required" >&2; exit 2; }
echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
    || { echo "docs-pdf.sh: version must be X.Y.Z, got '$VERSION'" >&2; exit 2; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCS_DIR="$ROOT/internal/docs"
OUT="${OUT:-$ROOT/dist}"

BUNDLE=(installation-bare-metal configuration hardening backup-restore upgrade)
LOCALES=(ru en)

# Число отдельно от массива: иначе самопроверка ниже сверяла бы PDF с тем
# же урезанным списком и не заметила бы пропавшую страницу.
[ "${#BUNDLE[@]}" -eq 5 ] \
    || { echo "docs-pdf.sh: bundle must have exactly 5 pages, got ${#BUNDLE[@]}" >&2; exit 1; }

for tool in pandoc xelatex pdftotext python3 kpsewhich fc-list; do
    command -v "$tool" >/dev/null 2>&1 \
        || { echo "docs-pdf.sh: '$tool' not found on PATH — install it before running this script" >&2; exit 1; }
done

kpsewhich fvextra.sty >/dev/null 2>&1 \
    || { echo "docs-pdf.sh: LaTeX package 'fvextra' not found (needed to wrap long code lines) — install it, e.g. 'tlmgr install fvextra'" >&2; exit 1; }

kpsewhich seqsplit.sty >/dev/null 2>&1 \
    || { echo "docs-pdf.sh: LaTeX package 'seqsplit' not found (needed so long inline code doesn't overrun table cells) — install it, e.g. 'tlmgr install seqsplit'" >&2; exit 1; }

kpsewhich hyph-ru.tex >/dev/null 2>&1 \
    || { echo "docs-pdf.sh: Russian hyphenation patterns not found (long Cyrillic words overrun table cells without them) — install them, e.g. 'tlmgr install hyphen-russian'" >&2; exit 1; }

# fc-list в пайп с grep -q рвётся SIGPIPE (pipefail видит его как провал) —
# сначала забираем вывод целиком, потом грепаем строку.
installed_fonts="$(fc-list)"
grep -qi 'DejaVu Sans' <<<"$installed_fonts" \
    || { echo "docs-pdf.sh: DejaVu fonts not found — install the DejaVu font family (Cyrillic needs it)" >&2; exit 1; }

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cat >"$WORK/preamble.tex" <<'EOF'
\usepackage{fvextra}
\DefineVerbatimEnvironment{Highlighting}{Verbatim}{breaklines,breakanywhere,commandchars=\\\{\}}
% Код без пробелов не переносится сам и в узких колонках таблиц наезжает
% на соседнюю — seqsplit даёт ему точки разрыва.
\usepackage{seqsplit}
\DeclareRobustCommand{\texttt}[1]{\seqsplit{#1}}
EOF

# Портирует internal/docs/anchors.go (транслитерация + дедуп якорей) на Python:
# Go-код не трогаем в этой задаче, поэтому источник анкоров дублируется намеренно.
cat >"$WORK/preprocess.py" <<'PYEOF'
import os
import re
import sys

CYR = {
    "а": "a", "б": "b", "в": "v", "г": "g", "д": "d", "е": "e", "ё": "e",
    "ж": "zh", "з": "z", "и": "i", "й": "y", "к": "k", "л": "l", "м": "m",
    "н": "n", "о": "o", "п": "p", "р": "r", "с": "s", "т": "t", "у": "u",
    "ф": "f", "х": "h", "ц": "c", "ч": "ch", "ш": "sh", "щ": "sch",
    "ъ": "", "ы": "y", "ь": "", "э": "e", "ю": "yu", "я": "ya",
}

HEADING_RE = re.compile(r"^(#{1,6})\s+(.*\S)\s*$")
FENCE_RE = re.compile(r"^\s*```")
CODE_SPAN_RE = re.compile(r"`([^`]*)`")
LINK_RE = re.compile(r"\[([^\]\[]*)\]\(/docs/([a-z0-9-]+)(#[a-z0-9-]+)?\)")
LOCAL_ANCHOR_RE = re.compile(r"\[([^\]\[]*)\]\(#([a-z0-9-]+)\)")


def slugify(text):
    out = []
    prev_dash = True
    for ch in text.lower():
        if ("a" <= ch <= "z") or ("0" <= ch <= "9"):
            out.append(ch)
            prev_dash = False
        elif CYR.get(ch):
            out.append(CYR[ch])
            prev_dash = False
        elif ch in ("ъ", "ь"):
            continue
        else:
            if not prev_dash:
                out.append("-")
                prev_dash = True
    return "".join(out).strip("-")


class IDGen:
    def __init__(self):
        self.used = set()

    def generate(self, text):
        slug = slugify(text) or "section"
        candidate = slug
        i = 1
        while candidate in self.used:
            candidate = f"{slug}-{i}"
            i += 1
        self.used.add(candidate)
        return candidate


def plain_text(heading):
    return CODE_SPAN_RE.sub(r"\1", heading)


def inject_heading_ids(slug, lines):
    gen = IDGen()
    ids = {}
    top_id = None
    top_text = None
    out = []
    in_fence = False
    for line in lines:
        if FENCE_RE.match(line):
            in_fence = not in_fence
            out.append(line)
            continue
        m = None if in_fence else HEADING_RE.match(line)
        if not m:
            out.append(line)
            continue
        text = plain_text(m.group(2))
        rid = gen.generate(text)
        ids[rid] = f"{slug}--{rid}"
        if top_id is None:
            top_id, top_text = rid, text
        out.append(f"{line} {{#{slug}--{rid}}}")
    return out, ids, top_id, top_text


def rewrite_links(current_slug, lines, bundle_ids, bundle_top):
    out = []
    in_fence = False
    for line in lines:
        if FENCE_RE.match(line):
            in_fence = not in_fence
            out.append(line)
            continue
        if in_fence:
            out.append(line)
            continue

        def repl(m):
            text, slug, anchor = m.group(1), m.group(2), m.group(3)
            if slug in bundle_ids:
                if anchor:
                    raw = anchor[1:]
                    gid = bundle_ids[slug].get(raw)
                    if gid is None:
                        sys.stderr.write(
                            f"docs-pdf: unknown anchor /docs/{slug}{anchor}\n"
                        )
                        sys.exit(1)
                else:
                    gid = f"{slug}--{bundle_top[slug]}"
                return f"[{text}](#{gid})"
            path = f"/docs/{slug}{anchor or ''}"
            return f"{text} (`{path}`)"

        # Локальные «#anchor» (без /docs/<slug>) в комплекте тоже требуют
        # префикса страницы, иначе hyperref не находит цель.
        def repl_local(m):
            text, raw = m.group(1), m.group(2)
            gid = bundle_ids[current_slug].get(raw)
            if gid is None:
                sys.stderr.write(f"docs-pdf: unknown anchor #{raw} in {current_slug}\n")
                sys.exit(1)
            return f"[{text}](#{gid})"

        # Сначала местные «#anchor» (иначе они бы поймали префикс, который
        # только что вписал LINK_RE в «(#slug--anchor)» соседних ссылок).
        line = LOCAL_ANCHOR_RE.sub(repl_local, line)
        line = LINK_RE.sub(repl, line)
        out.append(line)
    return out


def main():
    locale, docs_dir, work_dir = sys.argv[1], sys.argv[2], sys.argv[3]
    bundle = sys.argv[4:]

    raw = {}
    bundle_ids = {}
    bundle_top = {}
    top_text_by_slug = {}
    for slug in bundle:
        path = os.path.join(docs_dir, locale, slug + ".md")
        with open(path, encoding="utf-8") as fh:
            lines = fh.read().split("\n")
        out, ids, top_id, top_text = inject_heading_ids(slug, lines)
        raw[slug] = out
        bundle_ids[slug] = ids
        bundle_top[slug] = top_id
        top_text_by_slug[slug] = top_text

    manifest_lines = []
    for i, slug in enumerate(bundle):
        lines = rewrite_links(slug, raw[slug], bundle_ids, bundle_top)
        if i > 0:
            lines = ["\\newpage", ""] + lines
        with open(os.path.join(work_dir, slug + ".md"), "w", encoding="utf-8") as fh:
            fh.write("\n".join(lines))
        manifest_lines.append(f"{slug}\t{top_text_by_slug[slug]}")

    with open(os.path.join(work_dir, "headings.tsv"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(manifest_lines) + "\n")


main()
PYEOF

title_for() {
    case "$1" in
        ru) echo "Gotcha — установка без Docker (bare-metal)" ;;
        en) echo "Gotcha — bare-metal install (without Docker)" ;;
    esac
}

subtitle_for() {
    case "$1" in
        ru) echo "Комплект эксплуатационной документации, версия $VERSION" ;;
        en) echo "Operations documentation bundle, version $VERSION" ;;
    esac
}

verify_pdf() {
    local pdf="$1" manifest="$2" locale="$3" text slug heading

    [ -s "$pdf" ] || { echo "docs-pdf.sh: build produced no file: $pdf" >&2; return 1; }

    text="$(pdftotext "$pdf" - 2>/dev/null)" \
        || { echo "docs-pdf.sh: pdftotext could not read the text layer: $pdf" >&2; return 1; }
    [ -n "$text" ] || { echo "docs-pdf.sh: text layer is empty: $pdf" >&2; return 1; }

    while IFS=$'\t' read -r slug heading; do
        [ -n "$heading" ] || continue
        grep -qF "$heading" <<<"$text" \
            || { echo "docs-pdf.sh: heading not found for '$slug' in $pdf: $heading" >&2; return 1; }
    done <"$manifest"

    if [ "$locale" = ru ]; then
        grep -qP '[а-яА-ЯёЁ]' <<<"$text" \
            || { echo "docs-pdf.sh: no Cyrillic found in the Russian build: $pdf" >&2; return 1; }
    fi
}

for locale in "${LOCALES[@]}"; do
    loc_work="$WORK/$locale"
    mkdir -p "$loc_work"
    python3 "$WORK/preprocess.py" "$locale" "$DOCS_DIR" "$loc_work" "${BUNDLE[@]}"

    inputs=()
    for slug in "${BUNDLE[@]}"; do
        inputs+=("$loc_work/$slug.md")
    done

    pdf="$OUT/gotcha-bare-metal-$locale-$VERSION.pdf"

    pandoc "${inputs[@]}" \
        -o "$pdf" \
        -f 'markdown-raw_html+raw_tex' \
        --pdf-engine=xelatex \
        --include-in-header="$WORK/preamble.tex" \
        -V mainfont="DejaVu Sans" \
        -V monofont="DejaVu Sans Mono" \
        -V geometry:margin=2.5cm \
        -V lang="$locale" \
        -V colorlinks=true -V linkcolor=blue -V urlcolor=blue \
        --toc --toc-depth=2 \
        -M title="$(title_for "$locale")" \
        -M subtitle="$(subtitle_for "$locale")" \
        -M date="$(date -u +%Y-%m-%d)"

    verify_pdf "$pdf" "$loc_work/headings.tsv" "$locale"
    echo "docs-pdf.sh: built $pdf"
done
