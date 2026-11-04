# `gcpemu stop` reports success only for an exit it confirmed

Scripts and tests delete or reuse a data directory as soon as `gcpemu stop` returns, but nothing said what its success promised. #990 showed the gap: an Instance recorded its PID after it told clients it was up, so `stop` could find no process to wait for and report success while the Instance still wrote to its data directory. We decided that success means the Instance's process has exited and `stop` confirmed it, so nothing will write to the data directory again. `stop` takes the PID from the Instance it reached, through the Admin API, because a PID file may be left by an Instance that crashed. When it cannot confirm the exit, it fails: a process that has not exited by its limit is an error that names the PID, and when the host cannot watch the PID at all, as for a Containerized Instance, `stop` still asks the Instance to shut down and then exits non-zero and says the exit is unconfirmed. `stop` never kills the process. #998, #999 and #1000 bring the command in line with this decision.

## Considered options

- **Trust the PID file.** This is what `stop` did. A crash leaves the file behind, and it can name a dead process while a new Instance answers on the same port.
- **Treat a shutdown the Instance accepted as success.** It would give a Containerized Instance a friendlier result, but success would then mean two different things, and a script could not tell whether the data directory was safe to delete.
- **Wait with no limit.** It would honour the promise for any slow exit, but a stuck Instance would hang the script that called `stop`.
- **Kill a process that outlives the limit.** `stop` would then always end the process, but a killed Instance has not stopped cleanly, and a pid can be reused. Only reclaiming a Test data dir kills, and only a process named gcpemu (ADR 0057).

## Consequences

- A script that runs `gcpemu stop` against a Containerized Instance sees a non-zero exit even though the Instance accepted the shutdown.
- A stop can be a Clean stop and still have left things behind: an Instance logs what it could not remove (#920), and `stop` still succeeds.
