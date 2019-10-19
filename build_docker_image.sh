#!/bin/bash
if [ $# -ne 1 ];
then 
	echo "Usage $0 <tag>"
	exit
fi

docker build ./ -t gcr.io/cyral-dev/cyral-push-proxy:$1 -f Dockerfile.cyral

