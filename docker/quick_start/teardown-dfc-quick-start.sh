#!/usr/bin/env bash

container_id=`docker ps | grep "liangdrew/dfc" | awk '{ print $1 }'`
docker stop $container_id
docker rm $container_id