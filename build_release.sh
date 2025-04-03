#!/bin/bash

if [ "$#" != "4" ]; then 
    echo "$0"' $os $arch $filename $dir'
    echo "example:"
    echo "-- $0 windows amd64 tgfile-server ./cmd"
    echo "-- $0 linux amd64 tgfile-server ./cmd"
    exit 1
fi 

os="$1"
arch="$2"
filename="$3"
builddir="$4"
output="${filename}-${os}-${arch}"
bname="$output"
if [ "$os" == "windows" ]; then 
    bname="$bname.exe"
fi 

CGO_LDFLAGS="-static" CGO_ENABLED=0 GOOS=${os} GOARCH=${arch} go build -a -tags netgo -ldflags '-w' -o ${bname} ${builddir}
tar -czf "$output.tar.gz" "$bname"
rm $bname