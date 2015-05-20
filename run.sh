#!/bin/sh
mkdir -p /go/src/github.com/mergermarket/logspout-cloudwatchlogs
cp /logspout-cloudwatchlogs/logspout-cloudwatchlogs.go /go/src/github.com/mergermarket/logspout-cloudwatchlogs/logspout-cloudwatchlogs.go
cp /src/modules.go /go/src/github.com/gliderlabs/logspout/modules.go
cd /go/src/github.com/gliderlabs/logspout
export GOPATH=/go
go get
go build -ldflags "-X main.Version $1" -o /bin/logspout
if [ $? -eq 0 ]
then
    /bin/logspout "$@"
fi

