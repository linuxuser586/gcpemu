# `gcpemu env` names the Admin kubeconfig whether or not a cluster runs

`gcpemu env` exports `KUBECONFIG` as the Admin kubeconfig's path in the Instance's data directory (FR-CORE-004). That file holds exactly the clusters running now, so on a new Instance it does not exist yet, and after the last cluster is deleted it is empty. kubectl treats both like no kubeconfig at all and goes to `localhost:8080`. A tool that took `KUBECONFIG` being set as a sign of a cluster ran kubectl against nothing (#1168).

We decided that `KUBECONFIG` keeps naming the Admin kubeconfig whether or not a cluster runs. It says where the Instance's clusters will be, not that there are any. Variables are often read once and used after clusters are created: `env --shell github` writes them to `$GITHUB_ENV` for every later CI step, `gcpemu run` gives them to its child once, and a shell that ran `eval "$(gcpemu env)"` keeps them. A fixed path lets kubectl find a cluster that a later `tofu apply` or `gcloud container clusters create` brings up. A tool that needs to know whether a cluster runs asks the kubeconfig itself (`kubectl config get-contexts -o name` lists one context per running cluster), or asks GKE (`gcloud container clusters list --filter=status=RUNNING`).

## Considered options

- **Leave `KUBECONFIG` out until a cluster runs.** Being set would then mean there is a cluster, which is what #1168 expected. It was rejected because every flow that reads the variables once and creates a cluster afterwards would lose kubectl silently, and would have to call `env` again at the right moment.
- **Always write the kubeconfig, empty when no cluster runs.** It removes the missing file but changes nothing for kubectl, which still goes to `localhost:8080` with an empty kubeconfig.
- **Warn on stderr from `env` while no cluster runs.** It was rejected as noise: it would print on every `env` for Instances that do not use GKE.

## Consequences

- `env`'s help and the README say that `KUBECONFIG` being set does not mean a cluster exists, and how to check.
- The Admin kubeconfig must list exactly the running clusters at all times, including after a reset, for its contexts to be a true answer.
