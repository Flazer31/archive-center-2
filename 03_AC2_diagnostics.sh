#!/bin/sh
set -eu
package_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$package_dir/bin/ac2-updater" diagnostics --root "$package_dir" "$@"
