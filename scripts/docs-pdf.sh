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

# \scriptsize вместо основного шрифта — иначе безопасная ширина строки
# код-блока (MAX_LINE_LEN ниже) не вмещает реальные команды комплекта.
cat >"$WORK/preamble.tex" <<'EOF'
\usepackage{fvextra}
\DefineVerbatimEnvironment{Highlighting}{Verbatim}{breaklines,breakanywhere,commandchars=\\\{\},fontsize=\scriptsize}
\DefineVerbatimEnvironment{verbatim}{Verbatim}{breaklines,breakanywhere,fontsize=\scriptsize}
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

# Знаки, после которых код без пробелов (GOTCHA_LONG_NAME=value,
# internal/uptime/queue.go) может перенестись в узкой колонке таблицы.
BREAK_AFTER = set("_-./=:|,")
ZWSP = "​"

# Порог измерен бинарным поиском при текущих \scriptsize/2.5cm — без
# переноса умещается до 112 символов, 100 оставляет запас.
MAX_LINE_LEN = 100


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


def add_soft_breaks(lines):
    def repl(m):
        chars = []
        for ch in m.group(1):
            chars.append(ch)
            if ch in BREAK_AFTER:
                chars.append(ZWSP)
        return "`" + "".join(chars) + "`"

    out = []
    in_fence = False
    for line in lines:
        if FENCE_RE.match(line):
            in_fence = not in_fence
            out.append(line)
            continue
        out.append(line if in_fence else CODE_SPAN_RE.sub(repl, line))
    return out


def check_code_block_lines(path, lines):
    in_fence = False
    for i, line in enumerate(lines, start=1):
        if FENCE_RE.match(line):
            in_fence = not in_fence
            continue
        if not in_fence:
            continue
        if len(line) > MAX_LINE_LEN:
            sys.stderr.write(
                f"docs-pdf: {path}:{i}: code block line is {len(line)} chars, "
                f"over the {MAX_LINE_LEN}-char safe width — pandoc has to wrap it, "
                "and a wrapped code-block line silently loses or gains a character "
                "in the PDF text layer (truncation or an inserted space).\n"
                f"  line: {line}\n"
                "  fix: break the line yourself (e.g. a shell '\\' continuation) "
                "or shorten it (hoist a long value into a variable).\n"
            )
            sys.exit(1)


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
        check_code_block_lines(path, lines)
        out, ids, top_id, top_text = inject_heading_ids(slug, lines)
        raw[slug] = out
        bundle_ids[slug] = ids
        bundle_top[slug] = top_id
        top_text_by_slug[slug] = top_text

    manifest_lines = []
    for i, slug in enumerate(bundle):
        lines = rewrite_links(slug, raw[slug], bundle_ids, bundle_top)
        lines = add_soft_breaks(lines)
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

# Убирает декоративные значки fvextra (⌋/↪), футер-нумерацию, form feed
# и пустые строки — остальные пробелы не трогает, иначе не поймать вставленный.
clean_text_layer() {
    pdftotext "$1" - 2>/dev/null | python3 -c '
import sys
text = sys.stdin.read().replace("⌋", "").replace("↪", "").replace("\x0c", "")
noise = {"", "T", "1"}
sys.stdout.write("".join(l for l in text.split("\n") if l not in noise))
'
}

self_test_special_chars() {
    # Литеральные $/`/# ниже не должны раскрываться шеллом.
    # shellcheck disable=SC2016
    local sample='a_b^c#d&e%f\g{h}i$j~k|l,m.n/o-p_q=r:s'
    local src="$WORK/selftest-src" outdir="$WORK/selftest-out" log="$WORK/selftest.log"
    mkdir -p "$src/ru" "$outdir"
    # shellcheck disable=SC2016
    printf '# T\n\nCode: `%s`.\n' "$sample" >"$src/ru/torture.md"
    python3 "$WORK/preprocess.py" ru "$src" "$outdir" torture

    if ! pandoc "$outdir/torture.md" -o "$outdir/torture.pdf" \
        -f 'markdown-raw_html+raw_tex' --pdf-engine=xelatex \
        --include-in-header="$WORK/preamble.tex" \
        -V mainfont="DejaVu Sans" -V monofont="DejaVu Sans Mono" >"$log" 2>&1
    then
        echo "docs-pdf.sh: regression check failed — LaTeX chokes on a special character in inline code, fragment: $sample" >&2
        tail -15 "$log" >&2
        return 1
    fi

    local text
    text="$(pdftotext "$outdir/torture.pdf" - 2>/dev/null)" \
        || { echo "docs-pdf.sh: regression check: pdftotext failed on the special-character sample" >&2; return 1; }
    grep -qF "$sample" <<<"$text" \
        || { echo "docs-pdf.sh: regression check: special characters missing from rendered text: $sample" >&2; return 1; }
}

# Строит PDF из код-блока в обход preprocess.py — сам guard отказал бы
# строке длиннее MAX_LINE_LEN, а тут нужно увидеть, что сделает pandoc/xelatex.
build_raw_block() {
    local content="$1" lang="$2" outdir="$3"
    mkdir -p "$outdir"
    # shellcheck disable=SC2016
    printf '# T\n\n```%s\n%s\n```\n' "$lang" "$content" >"$outdir/raw.md"
    pandoc "$outdir/raw.md" -o "$outdir/raw.pdf" \
        -f 'markdown-raw_html+raw_tex' --pdf-engine=xelatex \
        --include-in-header="$WORK/preamble.tex" \
        -V mainfont="DejaVu Sans" -V monofont="DejaVu Sans Mono" >"$outdir/raw.log" 2>&1
}

self_test_wrap_truncates_tagged() {
    # 140-символьный сплошной токен (без пробелов/пунктуации) в тегированном
    # блоке — далеко за MAX_LINE_LEN=100.
    local token outdir="$WORK/selftest-wrap-tagged" cleaned
    token="$(python3 -c "import string; print((string.ascii_lowercase+string.digits)*6)" | cut -c1-140)"
    if ! build_raw_block "$token" "bash" "$outdir"; then
        echo "docs-pdf.sh: regression check failed — a 140-char tagged code block did not build" >&2
        tail -15 "$outdir/raw.log" >&2
        return 1
    fi
    cleaned="$(clean_text_layer "$outdir/raw.pdf")"
    [ "$cleaned" != "$token" ] \
        || { echo "docs-pdf.sh: regression check: expected a wrapped tagged line to corrupt the text layer, but it matched exactly — MAX_LINE_LEN may be stale, recalibrate before trusting it" >&2; return 1; }
}

self_test_wrap_inserts_space_untagged() {
    # Тот же токен, нетегированный блок: перенос вставляет ЛИШНИЙ пробел
    # посреди токена вместо обрезания — другой механизм порчи текста.
    local token outdir="$WORK/selftest-wrap-untagged" cleaned
    token="$(python3 -c "import string; print((string.ascii_lowercase+string.digits)*6)" | cut -c1-140)"
    if ! build_raw_block "$token" "" "$outdir"; then
        echo "docs-pdf.sh: regression check failed — a 140-char untagged code block did not build" >&2
        tail -15 "$outdir/raw.log" >&2
        return 1
    fi
    cleaned="$(clean_text_layer "$outdir/raw.pdf")"
    [ "$cleaned" != "$token" ] \
        || { echo "docs-pdf.sh: regression check: expected a wrapped untagged line to corrupt the text layer, but it matched exactly — MAX_LINE_LEN may be stale, recalibrate before trusting it" >&2; return 1; }
    local no_space_cleaned="${cleaned// /}"
    [ "$no_space_cleaned" = "$token" ] \
        || { echo "docs-pdf.sh: regression check: untagged wrap corruption changed shape — expected exactly one inserted space, got: $cleaned" >&2; return 1; }
}

self_test_over_length_line_refused() {
    # 101 символ — на 1 больше MAX_LINE_LEN. Гоняем оба вида блока (со
    # словами и пробелами, сплошным токеном) — не только частный случай.
    local mixed="echo \"gotcha bare-metal install, padded with spaces to exactly one hundred chars xxxxxxxxxxxxxxxxxxx\""
    local solid src outdir log
    solid="$(python3 -c "import string; print((string.ascii_lowercase+string.digits)*4)" | cut -c1-101)"

    src="$WORK/selftest-refuse-tagged" outdir="$WORK/selftest-refuse-tagged-out" log="$WORK/selftest-refuse-tagged.log"
    mkdir -p "$src/ru" "$outdir"
    # shellcheck disable=SC2016
    printf '# T\n\n```bash\n%s\n```\n' "$mixed" >"$src/ru/overlen.md"
    if python3 "$WORK/preprocess.py" ru "$src" "$outdir" overlen >"$log" 2>&1; then
        echo "docs-pdf.sh: regression check: a 101-char tagged code line (with spaces) was not refused" >&2
        return 1
    fi
    grep -qF "overlen.md" "$log" \
        || { echo "docs-pdf.sh: regression check: refusal message doesn't name the file:" >&2; cat "$log" >&2; return 1; }

    src="$WORK/selftest-refuse-untagged" outdir="$WORK/selftest-refuse-untagged-out" log="$WORK/selftest-refuse-untagged.log"
    mkdir -p "$src/ru" "$outdir"
    # shellcheck disable=SC2016
    printf '# T\n\n```\n%s\n```\n' "$solid" >"$src/ru/overlen.md"
    if python3 "$WORK/preprocess.py" ru "$src" "$outdir" overlen >"$log" 2>&1; then
        echo "docs-pdf.sh: regression check: a 101-char untagged code line (solid token) was not refused" >&2
        return 1
    fi
    grep -qF "overlen.md" "$log" \
        || { echo "docs-pdf.sh: regression check: refusal message doesn't name the file:" >&2; cat "$log" >&2; return 1; }
}

self_test_max_length_line_survives() {
    # Ровно 100 символов (MAX_LINE_LEN), смешанный контент — не переносится
    # вовсе и обязан дойти до текстового слоя посимвольно, пробелы включая.
    local line="echo \"gotcha bare-metal install, padded with spaces to exactly one hundred chars xxxxxxxxxxxxxxxxxx\""
    local outdir="$WORK/selftest-maxlen" cleaned
    if ! build_raw_block "$line" "bash" "$outdir"; then
        echo "docs-pdf.sh: regression check failed — a 100-char tagged code block did not build" >&2
        tail -15 "$outdir/raw.log" >&2
        return 1
    fi
    cleaned="$(clean_text_layer "$outdir/raw.pdf")"
    [ "$cleaned" = "$line" ] \
        || { echo "docs-pdf.sh: regression check: a $(printf '%s' "$line" | wc -c)-char line at the safe width was altered in the text layer, expected: $line got: $cleaned" >&2; return 1; }
}

self_test_special_chars
self_test_wrap_truncates_tagged
self_test_wrap_inserts_space_untagged
self_test_over_length_line_refused
self_test_max_length_line_survives

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
