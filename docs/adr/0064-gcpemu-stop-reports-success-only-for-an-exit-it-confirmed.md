# `gcpemu stop` reports success only for an exit it confirmed

Scripts and tests delete or reuse a data directory as soon as `gcpemu stop` returns, but nothing said what its success promised. #990 showed the gap: an Instance recorded its PID after it told clients it was up, so `stop` could find no process to wait for and report success while the Instance still wrote to its data directory. We decided that success means the Instance's process has exited and `stop` confirmed it, so nothing will write to the data directory again. `stop` takes the PID from the Instance it reached, through the Admin API, because a PID file may be left by an Instance that crashed. When it cannot confirm the exit, it fails: a process that has not exited by its limit is an error that names the PID, and when the host cannot watch the PID at all, as for a Containerized Instance, `stop` still asks the Instance to shut down and then exits non-zero and says the exit is unconfirmed. `stop` never kills the process. #998, #999 and #1000 bring the command in line with this decision.

`stop` can watch a PID only where that PID means the same thing to it, so the Instance reports a PID scope with its PID, an opaque value that `stop` compares with its own: on Linux the PID namespace and the kernel's boot ID, on any other OS the OS's name. A scope that differs or is missing makes an Unconfirmed stop, and so does PID 1 in `stop`'s own scope: that is the container `stop` runs in, which it cannot outlive.

## Considered options

- **Trust the PID file.** This is what `stop` did. A crash leaves the file behind, and it can name a dead process while a new Instance answers on the same port.
- **Treat a shutdown the Instance accepted as success.** It would give a Containerized Instance a friendlier result, but success would then mean two different things, and a script could not tell whether the data directory was safe to delete.
- **Have the Instance report whether it is Containerized.** It uses the glossary's term, but an Instance in a container on the host's network is not a Containerized Instance and still has its own PIDs, so `stop` would watch an unrelated process. An Instance too old to report it would look watchable too.
- **Compare PID namespaces alone.** Every Linux kernel's first PID namespace reads the same, so a `stop` on the host could take a PID in a VM's for one of its own and report success. The boot ID tells them apart.
- **Wait with no limit.** It would honour the promise for any slow exit, but a stuck Instance would hang the script that called `stop`.
- **Kill a process that outlives the limit.** `stop` would then always end the process, but a killed Instance has not stopped cleanly, and a pid can be reused. Only reclaiming a Test data dir kills, and only a process named gcpemu (ADR 0057).

## Consequences

- A script that runs `gcpemu stop` against a Containerized Instance sees a non-zero exit even though the Instance accepted the shutdown.
- An Instance too old to report its PID scope gets an Unconfirmed stop even on the host, until it restarts on a newer version.
- An Unconfirmed stop exits with the same status as any other failure: only its message tells them apart.
- A stop can be a Clean stop and still have left things behind: an Instance logs what it could not remove (#920), and `stop` still succeeds.
