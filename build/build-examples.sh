#!/bin/bash
# Build a statically linked Cockroach binary
#
# Author: Peter Mattis (peter@cockroachlabs.com)

set -euo pipefail

# This is mildly tricky: This script runs itself recursively. The
# first time it is run it does not take the if-branch below and
# executes on the host computer. It uses the builder.sh script to run
# itself inside of docker passing "docker" as the argument causing the
# commands in the if-branch to be executed within the docker
# container.
if [ "${1-}" = "docker" ]; then
    time go get -v ./...
    time make STATIC=1 block_writer

    # Make sure the created binary is statically linked.  Seems
    # awkward to do this programmatically, but this should work.
    file cockroach | grep -F 'statically linked' > /dev/null
    exit 0
fi

# Build the cockroach and test binaries.
$(dirname $0)/builder.sh $0 docker