#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ -n "${GO:-}" ]; then
    go_bin=$GO
elif command -v go >/dev/null 2>&1; then
    go_bin=$(command -v go)
elif [ -x "$project_root/../.tools/go/bin/go" ]; then
    go_bin="$project_root/../.tools/go/bin/go"
elif [ -f "$project_root/../.tools/go/bin/go.exe" ]; then
    go_bin="$project_root/../.tools/go/bin/go.exe"
else
    printf '%s\n' 'Go not found. Set GO to the compiler executable path.' >&2
    exit 1
fi

cd "$project_root"
exec "$go_bin" run ./cmd/packager build -source "$project_root" -go "$go_bin" "$@"
