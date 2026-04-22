#!/usr/bin/env bash
set -euo pipefail

VERBOSE=0
CLEAN=0
TIDY=0
GENERATE=0
TARGET=""

usage() {
    echo "Usage: $0 [OPTIONS] [TARGET]"
    echo ""
    echo "Options:"
    echo "  -v, --verbose    Show full build output"
    echo "  --clean          Remove existing binaries before building"
    echo "  --tidy           Run go mod tidy before building"
    echo "  --generate       Run go generate before building"
    echo "  -h, --help       Show this help"
    echo ""
    echo "Targets:"
    echo "  (none)           Build all targets"
    echo "  dave             Build main binary only"
    echo "  img-mcp          Build specific MCP server"
    echo "  <name>           Build by target name"
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -v|--verbose)
            VERBOSE=1
            shift
            ;;
        --clean)
            CLEAN=1
            shift
            ;;
        --tidy)
            TIDY=1
            shift
            ;;
        --generate)
            GENERATE=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        -*)
            echo "Unknown flag: $1"
            usage
            exit 1
            ;;
        *)
            if [[ -n "$TARGET" ]]; then
                echo "Error: multiple targets specified"
                usage
                exit 1
            fi
            TARGET="$1"
            shift
            ;;
    esac
done

log() {
    if [[ $VERBOSE -eq 1 ]]; then
        "$@"
    else
        "$@" > /dev/null 2>&1
    fi
}

declare -a t_names=()
declare -a t_srcs=()
declare -a t_outs=()

add_target() {
    t_names+=("$1")
    t_srcs+=("$2")
    t_outs+=("$3")
}

add_target "dave" "." "dave"

for mainfile in mcps/*/main.go; do
    [[ -f "$mainfile" ]] || continue
    name=$(basename "$(dirname "$mainfile")")
    add_target "$name" "./mcps/$name" "mcps/$name/$name"
done

for mainfile in cmd/*/main.go; do
    [[ -f "$mainfile" ]] || continue
    name=$(basename "$(dirname "$mainfile")")
    add_target "$name" "./cmd/$name" "cmd/$name/$name"
done

build_target() {
    local name="$1"
    local src="$2"
    local out="$3"

    if [[ -n "$TARGET" && "$TARGET" != "$name" ]]; then
        return 0
    fi

    printf "  %-20s " "$name"

    if [[ $CLEAN -eq 1 && -f "$out" ]]; then
        rm -f "$out"
    fi

    if log go build -o "$out" "$src"; then
        echo "OK"
        return 0
    else
        echo "FAILED"
        return 1
    fi
}

if [[ -n "$TARGET" ]]; then
    found=0
    for i in "${!t_names[@]}"; do
        if [[ "${t_names[$i]}" == "$TARGET" ]]; then
            found=1
            break
        fi
    done
    if [[ $found -eq 0 ]]; then
        echo "Error: unknown target '$TARGET'"
        echo ""
        echo "Available targets:"
        for name in "${t_names[@]}"; do
            echo "  $name"
        done
        exit 1
    fi
fi

echo "Building dave..."

if [[ $TIDY -eq 1 ]]; then
    printf "  %-20s " "go mod tidy"
    if log go mod tidy; then
        echo "OK"
    else
        echo "FAILED"
        exit 1
    fi
fi

if [[ $GENERATE -eq 1 ]]; then
    printf "  %-20s " "go generate"
    if log go generate ./...; then
        echo "OK"
    else
        echo "FAILED"
        exit 1
    fi
fi

failed=0
built=0

for i in "${!t_names[@]}"; do
    if build_target "${t_names[$i]}" "${t_srcs[$i]}" "${t_outs[$i]}"; then
        ((built++)) || true
    else
        ((failed++)) || true
    fi
done

echo ""
if [[ $failed -gt 0 ]]; then
    echo "Done: $built succeeded, $failed failed"
    exit 1
else
    echo "Done: $built targets built successfully"
fi
