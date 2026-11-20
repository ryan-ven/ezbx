#!/bin/sh
# Self-test for scripts/privacy-scan.sh.
#
# Builds a throwaway git repository, adds files that must be blocked and files
# that must pass, and checks the scanner's answer for each. The bad samples are
# assembled from pieces at run time, so this file itself contains none of them.
#
#   sh scripts/privacy-scan-test.sh

SCANNER="$(cd "$(dirname "$0")" && pwd)/privacy-scan.sh"
WORK=$(mktemp -d) || exit 2
trap 'rm -rf "$WORK"' EXIT
failures=0

rep() { printf "%${2}s" "" | tr ' ' "$1"; }
A30=$(rep A 30)
A40=$(rep A 40)
BS='\'
DRIVE='X:'
AT='@'
U=Users
H=home

new_repo() {
  rm -rf "$WORK/r" && mkdir -p "$WORK/r/scripts" && cd "$WORK/r" || exit 2
  git init -q && git config user.name Test && git config user.email "test${AT}example.com"
  git config core.autocrlf false && git config commit.gpgsign false
  cp "$SCANNER" scripts/privacy-scan.sh
}

# expect blocked|passes MODE DESCRIPTION
expect() {
  want=$1 mode=$2 what=$3
  out=$(EZBX_PRIVATE_TERMS="zebrafish" sh scripts/privacy-scan.sh "$mode" 2>&1)
  code=$?
  if { [ "$want" = blocked ] && [ "$code" -eq 1 ]; } || { [ "$want" = passes ] && [ "$code" -eq 0 ]; }; then
    echo "ok    $what"
  else
    echo "FAIL  $what (wanted $want, exit $code)"
    printf '%s\n' "$out" | sed 's/^/      /'
    failures=$((failures + 1))
  fi
}

# One bad line per case, each checked in its own repository.
bad_line() {
  new_repo
  printf '%s\n' "$2" > sample.txt
  git add -A
  expect blocked --staged "$1"
}

bad_line "OpenAI-style key"            "key sk-$A30"
bad_line "Stripe key"                  "key sk_live_$A30"
bad_line "Stripe restricted key"       "key rk_live_$A30"
bad_line "GitHub token"                "token ghp_$A30"
bad_line "GitLab token"                "token glpat-$A30"
bad_line "Slack token"                 "token xo""xb-1234567890-$A30"
bad_line "Hugging Face token"          "token hf_$A30"
bad_line "npm token"                   "token npm_$A40"
bad_line "Google key"                  "key AIza${A30}12345"
bad_line "AWS key id"                  "id AKIA$(rep A 16)"
bad_line "JWT"                         "jwt eyJ$(rep a 12).eyJ$(rep b 12).sig"
bad_line "private key block"           "-----BEGIN RSA PRIVATE ""KEY-----"
bad_line "generic quoted secret"       "api_key = \"$(rep 9 24)\""
bad_line "email address"               "mail someone${AT}gmail.com"
bad_line "drive path with backslash"   "path ${DRIVE}${BS}Users${BS}someone"
bad_line "drive path with slash"       "path ${DRIVE}/${U}/someone"
bad_line "network share path"          "path ${BS}${BS}fileserver${BS}share"
bad_line "Git Bash drive path"         "path /c/${U}/someone"
bad_line "macOS home path"             "path /${U}/someone/x"
bad_line "Linux home path"             "path /${H}/someone/x"
bad_line "private term"                "a Zebrafish here"
bad_line "allowed word beside a key"   "see users.noreply.github.com and sk-$A30"

new_repo
printf 'key sk-%s\n' "$A30" | iconv -f UTF-8 -t UTF-16 > utf16.txt
git add -A
expect blocked --staged "UTF-16 file with a key"

new_repo
printf 'a\000b\n' > data.bin
git add -A
expect blocked --staged "unknown binary file"

new_repo
printf '\211PNG\r\n\032\n\000\000' > logo.png
git add -A
expect passes --staged "image file"

new_repo
printf 'API_KEY=x\n' > .env
git add -A
expect blocked --staged "env file"

new_repo
echo "{}" > .npmrc
git add -A
expect blocked --staged "npm credentials file"

# Renames must be checked, both for new content and for the new name.
new_repo
seq 1 200 > notes.txt && git add -A && git commit -qm base
git mv notes.txt renamed.txt && echo "key sk-$A30" >> renamed.txt && git add -A
expect blocked --staged "renamed file with a key"
new_repo
seq 1 200 > notes.txt && git add -A && git commit -qm base
git mv notes.txt .env
expect blocked --staged "file renamed to .env"

# A secret removed in a later commit is gone from the files but not from history.
new_repo
echo "key sk-$A30" > old.txt && git add -A && git commit -qm one
git rm -q old.txt && git commit -qm two
expect passes  --all     "removed secret is not in current files"
expect blocked --history "removed secret is still found in history"

new_repo
echo hi > a.txt && git add -A && git commit -qm "mail me at someone${AT}gmail.com"
expect blocked --history "email in a commit message"

new_repo
echo hi > a.txt && git add -A && git -c user.email="someone${AT}gmail.com" commit -qm one
expect blocked --history "real email as commit author"

# Things that look similar but are fine.
new_repo
cat > fine.txt <<EOF
See https://github.com/example/project and http://127.0.0.1:8080/v1
Clone with git${AT}github.com:example/project.git
Placeholder path ${DRIVE}${BS}Users${BS}Example${BS}project
Inside the box the home is /home/agent
Package @scope/some-tool${AT}1.2.3
"apiKey": "\${gateway.key}"
A task-runner-configuration-name is not a key
Author 123+someone${AT}users.noreply.github.com
Mail test${AT}example.com
EOF
git add -A
expect passes --staged "lookalikes that are fine"

new_repo
echo "key sk-$A30 privacy-ok" > marked.txt
git add -A
expect passes --staged "line marked privacy-ok"

echo ""
if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed."
  exit 1
fi
echo "All privacy scanner cases passed."
