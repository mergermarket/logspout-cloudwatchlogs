#!/bin/bash

set -e

docker run \
    -v $PWD:/logspout-cloudwatchlogs \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -i -t --entrypoint=/bin/sh \
    --rm \
    --name logspout-cloudwatchlogs \
    gliderlabs/logspout:master


