#!/bin/sh
# Privacy and secret scanner.
#
#   scripts/privacy-scan.sh --staged   check what is about to be committed (default)
#   scripts/privacy-scan.sh --all      check every tracked file (used by CI)
#
# Blocks: env files, key files, local-only files, agent files, API-key shapes,
# private keys, email addresses, personal Windows paths, and any word listed in
# .githooks/private-terms.txt (a gitignored, per-person file).
#
# To allow a line on purpose, add the marker  privacy-ok  to that line.

MODE="${1:---staged}"
fail=0

report() {
  echo "BLOCKED: $1"
  fail=1
}

# Prints the content of a file as it will be committed (staged copy),
# or as it is on disk when scanning everything.
read_file() {
  if [ "$MODE" = "--all" ]; then
    cat "$1"
  else
    git show ":$1"
  fi
}

if [ "$MODE" = "--all" ]; then
  FILES=$(git ls-files)
else
  FILES=$(git diff --cached --name-only --diff-filter=ACM)
fi

[ -z "$FILES" ] && exit 0

ROOT=$(git rev-parse --show-toplevel)
TERMS="$ROOT/.githooks/private-terms.txt"

# Regular expressions for things that must never be published.
PATTERNS='[A-Za-z]:\\(Users|My Workspace|Docker|LLM)|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}'
# Matches that are fine (placeholders and GitHub's no-reply addresses).
ALLOWED='privacy-ok|Users[\\/]Example|@example\.|noreply|@localhost'

# ---------- 1. Forbidden file names ----------
OLDIFS=$IFS
IFS='
'
for f in $FILES; do
  case "$f" in
    *.example|*.sample) ;;
    .env|*/.env|.env.*|*/.env.*) report "environment file: $f" ;;
    *.pem|*.key|*.pfx|*.p12|*id_rsa*|*id_ed25519*|*.kdbx) report "key file: $f" ;;
    backups/*|pi-config/*|harness-config/*|config/local*|*.local.json|*.local.toml|*.local.yaml)
      report "local-only file: $f" ;;
    agents/*|*/agents/*|AGENTS.md|*/AGENTS.md|CLAUDE.md|*/CLAUDE.md|GEMINI.md|.claude/*|HANDOFF*)
      report "agent or private file: $f" ;;
  esac
done

# ---------- 2. File contents ----------
for f in $FILES; do
  case "$f" in
    scripts/privacy-scan.sh|.githooks/private-terms.txt) continue ;;
  esac

  [ "$MODE" = "--all" ] && { [ -f "$f" ] || continue; }

  hits=$(read_file "$f" | grep -InE --label="$f" -H "$PATTERNS" | grep -vE "$ALLOWED")
  if [ -n "$hits" ]; then
    report "secret-like or personal text in $f"
    echo "$hits" | sed 's/^/    /' | cut -c1-160
  fi

  if [ -f "$TERMS" ]; then
    hits=$(read_file "$f" | grep -InFi --label="$f" -H -f "$TERMS" | grep -v 'privacy-ok')
    if [ -n "$hits" ]; then
      report "private term found in $f"
      echo "$hits" | sed 's/^/    /' | cut -c1-160
    fi
  fi
done
IFS=$OLDIFS

if [ "$fail" -ne 0 ]; then
  echo ""
  echo "Commit stopped. Remove the items above, or add 'privacy-ok' to a line that is safe on purpose."
  exit 1
fi

echo "Privacy scan passed."
exit 0
