#!/usr/bin/env bash

echo "Terminating tier 0 DFC cluster..."
make kill
make rmcache
make clean
rm -r /tmp/dfc
rm -r ~/.dfc*

echo "Terminating tier 1 DFC cluster..."
../docker/quick_start/teardown-dfc-quick-start.sh

echo "Deploying new tier 0 DFC cluster (proxy on port 8080)..."
printf '1\n 1\n 1\n 1\n' | make deploy

echo "Deploying new tier 1 DFC cluster (proxy on port 8082)..."
export PORT=8082
docker run -di -v ~/.aws/credentials:/root/.aws/credentials \
               -v ~/.aws/config:/root/.aws/config \
               -p 8082:8082 liangdrew/dfc
./../docker/quick_start/quick_start_dfc.sh
unset PORT