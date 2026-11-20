#!/bin/sh
# Privacy and secret scanner.
#
#   scripts/privacy-scan.sh --staged    check what is about to be committed (default)
#   scripts/privacy-scan.sh --all       check every tracked file as it is on disk
#   scripts/privacy-scan.sh --history   check every file version and every commit
#                                       message in the whole history (used by CI)
#
# Blocks: env, key and credential files; local-only and agent files; API-key
# shapes and private keys; email addresses; personal paths (drive letters,
# network shares, Git Bash drives, home folders); binary files that are not
# known media; and any word in the private terms list.
#
# Private terms come from .githooks/private-terms.txt (gitignored, one per
# line). If that file is missing, they come from the EZBX_PRIVATE_TERMS
# environment variable, which CI fills from a repository secret.
#
# To allow a line on purpose, add the marker  privacy-ok  to that line.
# The self-test is scripts/privacy-scan-test.sh.

MODE="${1:---staged}"
case "$MODE" in
  --staged|--all|--history) ;;
  *) echo "usage: $0 [--staged|--all|--history]"; exit 2 ;;
esac

fail=0
TAB=$(printf '\t')
SQ="'"
ROOT=$(git rev-parse --show-toplevel) || exit 2
cd "$ROOT" || exit 2

report() {
  echo "BLOCKED: $1"
  fail=1
}

# ---------- What to look for ----------

# Keys and tokens. The leading group keeps "task-runner-..." from looking like "sk-...".
B='(^|[^A-Za-z0-9_])'
P_KEYS="${B}sk[-_][A-Za-z0-9_-]{16,}|${B}(rk|pk)_(live|test)_[A-Za-z0-9]{10,}|${B}gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|${B}hf_[A-Za-z0-9]{30,}|${B}npm_[A-Za-z0-9]{30,}|AIza[0-9A-Za-z_-]{35}|(AKIA|ASIA)[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}[.]eyJ[A-Za-z0-9_-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----"

# Personal paths. Each pattern runs to the end of the path so that the
# placeholders below can be recognised as a whole.
NOTEND="[^[:space:]\"$SQ<>|,;)]*"
P_PATHS="(^|[^A-Za-z0-9])[A-Za-z]:(\\\\+|/)[A-Za-z]$NOTEND|\\\\\\\\[A-Za-z0-9._\$-]+\\\\[A-Za-z0-9]$NOTEND|(^|[^A-Za-z0-9.])/(mnt/)?[a-z]/Users/$NOTEND|/Users/[A-Za-z]$NOTEND|/home/[A-Za-z]$NOTEND"

P_EMAIL='[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+[.][A-Za-z]{2,}'

PATTERNS="$P_KEYS|$P_PATHS|$P_EMAIL"

# Secret-looking assignments such as api_key = "...". Checked without regard to case.
# Values starting with $, < or { are placeholders and are fine.
P_ASSIGN="(api[_-]?key|secret|token|passw(or)?d)[\"$SQ]?[[:space:]]*[:=][[:space:]]*[\"$SQ][^\"$SQ\$<{[:space:]]{12,}[\"$SQ]"

# Matches that are fine. Each one is checked against the matched text only,
# never the whole line, so a harmless word cannot excuse a real secret beside it.
ALLOWED='users[.]noreply[.]github[.]com|@example[.](com|org|net)|@localhost|^[^A-Za-z0-9.]?[A-Za-z]:\\+Users\\+Example|/Users/Example|/home/example|/home/agent|git@github[.]com'

# Binary files allowed without a content scan (images and fonts).
is_media() {
  case "$1" in
    *.png|*.jpg|*.jpeg|*.gif|*.ico|*.webp|*.woff|*.woff2) return 0 ;;
  esac
  return 1
}

# ---------- Private terms ----------

TERMS=""
TMPTERMS=$(mktemp) || exit 2
trap 'rm -f "$TMPTERMS"' EXIT
if [ -f "$ROOT/.githooks/private-terms.txt" ]; then
  tr -d '\r' < "$ROOT/.githooks/private-terms.txt" | grep -v '^[[:space:]]*$' > "$TMPTERMS"
elif [ -n "${EZBX_PRIVATE_TERMS:-}" ]; then
  printf '%s\n' "$EZBX_PRIVATE_TERMS" | tr -d '\r' | grep -v '^[[:space:]]*$' > "$TMPTERMS"
fi
if [ -s "$TMPTERMS" ]; then
  TERMS=$TMPTERMS
else
  echo "note: no private terms list was found, so that check was skipped."
fi

# ---------- What to scan ----------
# Each entry is SOURCE<TAB>PATH. SOURCE is "disk" or a git object name.

case "$MODE" in
  --all)
    ENTRIES=$(git ls-files | sed "s/^/disk$TAB/")
    ;;
  --staged)
    # R and T are included so that renamed files are checked too.
    ENTRIES=$(git diff --cached --name-only --diff-filter=ACMRT | sed "s/^\(.*\)$/:\1$TAB\1/")
    ;;
  --history)
    ENTRIES=$(git rev-list --all --objects \
      | git cat-file --batch-check='%(objecttype) %(objectname) %(rest)' \
      | sed -n "s/^blob \([0-9a-f]*\) \(..*\)$/\1$TAB\2/p" | sort -u)
    ;;
esac

read_src() {
  if [ "$1" = disk ]; then cat "$2"; else git cat-file blob "$1"; fi
}

# Prints text, utf16 or binary.
kind_of() {
  bom=$(read_src "$1" "$2" | head -c 2 | od -An -tx1 | tr -d ' \n')
  case "$bom" in fffe|feff) echo utf16; return ;; esac
  all=$(read_src "$1" "$2" | wc -c)
  nonul=$(read_src "$1" "$2" | tr -d '\000' | wc -c)
  if [ "$all" -ne "$nonul" ]; then echo binary; else echo text; fi
}

# Reads text on stdin and prints the offending lines as LABEL:LINE:TEXT.
scan_text() {
  label=$1
  text=$(cat)
  printf '%s\n' "$text" | grep -nE "$PATTERNS" | grep -v 'privacy-ok' | while IFS= read -r line; do
    bad=$(printf '%s\n' "${line#*:}" | grep -oE "$PATTERNS" | sed 's/^[^A-Za-z0-9\\/-]//' | grep -vE "$ALLOWED")
    [ -n "$bad" ] && printf '%s:%s\n' "$label" "$line"
  done
  printf '%s\n' "$text" | grep -niE "$P_ASSIGN" | grep -v 'privacy-ok' | sed "s|^|$label:|"
  if [ -n "$TERMS" ]; then
    printf '%s\n' "$text" | grep -niF -f "$TERMS" | grep -v 'privacy-ok' | sed "s|^|$label:(private term) |"
  fi
}

show() {
  printf '%s\n' "$1" | sort -u | sed 's/^/    /' | cut -c1-160
}

OLDIFS=$IFS
IFS='
'

# ---------- 1. Forbidden file names ----------
for entry in $ENTRIES; do
  f=${entry#*"$TAB"}
  case "$f" in
    *.example|*.sample) ;;
    .env|*/.env|.env.*|*/.env.*) report "environment file: $f" ;;
    *.pem|*.key|*.pfx|*.p12|*.ppk|*.jks|*.keystore|*id_rsa*|*id_ed25519*|*.kdbx) report "key file: $f" ;;
    .npmrc|*/.npmrc|.pypirc|*/.pypirc|.netrc|*/.netrc|*credentials*.json) report "credential file: $f" ;;
    backups/*|pi-config/*|harness-config/*|config/local*|*.local.json|*.local.toml|*.local.yaml)
      report "local-only file: $f" ;;
    agents/*|*/agents/*|AGENTS.md|*/AGENTS.md|CLAUDE.md|*/CLAUDE.md|GEMINI.md|.claude/*|HANDOFF*)
      report "agent or private file: $f" ;;
  esac
done

# ---------- 2. File contents ----------
for entry in $ENTRIES; do
  src=${entry%%"$TAB"*}
  f=${entry#*"$TAB"}
  case "$f" in
    scripts/privacy-scan.sh|.githooks/private-terms.txt) continue ;;
  esac
  [ "$src" = disk ] && { [ -f "$f" ] || continue; }

  case "$(kind_of "$src" "$f")" in
    utf16)
      hits=$(read_src "$src" "$f" | iconv -f UTF-16 -t UTF-8 2>/dev/null | scan_text "$f")
      ;;
    binary)
      is_media "$f" && continue
      report "binary file that cannot be checked: $f (save text as UTF-8; images and fonts are allowed)"
      continue
      ;;
    *)
      hits=$(read_src "$src" "$f" | scan_text "$f")
      ;;
  esac
  if [ -n "$hits" ]; then
    report "secret-like or personal text in $f"
    show "$hits"
  fi
done

# ---------- 3. Commit messages and identities (history only) ----------
if [ "$MODE" = "--history" ]; then
  hits=$(git log --all --format='%h %an <%ae> %cn <%ce>%n%B' | scan_text "commits")
  if [ -n "$hits" ]; then
    report "secret-like or personal text in commit messages or identities"
    show "$hits"
  fi
fi
IFS=$OLDIFS

if [ "$fail" -ne 0 ]; then
  echo ""
  echo "Stopped. Remove the items above, or add 'privacy-ok' to a line that is safe on purpose."
  exit 1
fi

echo "Privacy scan passed ($MODE)."
exit 0
