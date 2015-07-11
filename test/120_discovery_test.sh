#! /bin/bash

. ./config.sh

C1=10.2.1.41
C3=10.2.1.71

launch_all() {
    weave_on $HOST1 launch-router $1
    weave_on $HOST2 launch-router $1 $HOST1
    weave_on $HOST3 launch-router $1 $HOST2
}

start_suite "Peer discovery and multi-hop routing"

launch_all

start_container $HOST1 $C1/24 --name=c1
start_container $HOST3 $C3/24 --name=c3

assert_raises "exec_on $HOST1 c1 $PING $C3"
stop_router_on $HOST2
assert_raises "exec_on $HOST1 c1 $PING $C3"

stop_router_on $HOST1
stop_router_on $HOST3

launch_all --no-discovery

assert_raises "exec_on $HOST1 c1 $PING $C3"
stop_router_on $HOST2
assert_raises "exec_on $HOST1 c1 sh -c '! $PING $C3'"

end_suite
