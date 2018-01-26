#!/bin/bash -e

s3cmd get "${CONF_S3_URI}" /etc/mackerel-agent/mackerel-agent.conf --force

mkdir /var/lib/mackerel-agent
if s3cmd get "${ID_S3_URI}" /var/lib/mackerel-agent/id ; then
  : do_nothing
else
  mackerel-agent &
  sleep 1
  pkill mackerel-agent
  wait
  s3cmd put /var/lib/mackerel-agent/id ${ID_S3_URI}
fi

exec mackerel-agent
