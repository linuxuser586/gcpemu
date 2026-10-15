# Forwarding rule env vars are qualified, with a short name while unique

Each forwarding rule's host address was the Service endpoint `lb:<name>`, printed by `gcpemu env` as `GCPEMU_LB_<NAME>`. A global and a regional rule, or rules in two projects, can share a name, and then overwrote each other's endpoint (#314). The endpoint is now `lb:<project>/<region|global>/<name>`, which depends only on the rule itself, like the resource path and GKE's `gke:<cluster>/<namespace>/<service>/<port>`. `gcpemu env` always prints it as `GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME>`, and also prints `GCPEMU_LB_<NAME>` while exactly one rule across all projects and scopes has that name. A variable that would be ambiguous is never published: when a second rule takes a name, the short variable disappears instead of pointing at either rule, and the same goes for two qualified keys that flatten to one variable (`-` and `/` both become `_`). The data plane logs a warning when a collision appears, and `gcpemu doctor` lists it.

## Considered options

- **Always qualified, no short name.** Consistent, but it breaks every script and doc that uses `GCPEMU_LB_<NAME>` for the common case of one project with unique names.
- **Short until a collision, then qualified.** Creating a second rule would rename the first rule's variable, so a working script silently breaks.
- **Only the qualifiers needed to disambiguate.** The key would change as other rules come and go, for the same reason.

## Consequences

- The endpoint registry holds only qualified keys; the short variable is derived when env vars are printed, so no alias can outlive its rule, and the Web console and Admin API show only qualified keys.
- A rule's endpoint is deleted whenever its listener closes, on the host as in a Containerized Instance.
