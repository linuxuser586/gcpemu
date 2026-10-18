# Recovery from a runtime restart

The emulator does not bring Disrupted containers back by itself. When the
container runtime stops or restarts an Instance's Managed containers
without the Instance doing it (a Docker daemon restart, `colima restart`,
a Docker Desktop update, a VM runtime waking from sleep), the Instance
reports them and the user stops and starts the Instance.

`/_emu/v1/ready` fails while there is a Disrupted container, naming each
one and why ("exited", "dead" or "restarted"), and saying to stop and
start the Instance. That covers every Managed container the Instance
started, the VM relay included. Starting the Instance again rebuilds
everything from its state: a new VM relay listens for the same endpoints
on the same addresses and serves the Offered ports again, and a
Persistent Instance keeps its data.

Reset is not the remedy. It removes all resources and data, which is far
more than a lost container calls for.

## Why this is out of scope

Recovery is only worth having if it covers every Managed container, and
each one has its own state to restore after a restart behind the
Instance's back:

- Cloud SQL's Postgres, its agent and the address it holds
- GKE nodes, their pods and the cluster's server
- the Edge relay and the load balancer's data plane
- the NAT egress proxy
- the VM relay, whose agent forgets its listeners and Offered ports, and
  whose published control port can move to a different host port

None of them has a restart policy, and none of the Services is written to
notice that a container came back with lost state. Recovering one of
them alone restores nothing a user can see: the relay coming back does
not help while the containers it relays to are still down.

Stopping and starting the Instance already does all of this in one place
the code depends on anyway, so we chose detection (#318) over per-Service
recovery. A proposal to change that should cover every Managed container
and come with an ADR.

## Prior requests

- #411: "VM relay: does not recover after the VM runtime restarts (container stays down; listeners and offers lost)"
