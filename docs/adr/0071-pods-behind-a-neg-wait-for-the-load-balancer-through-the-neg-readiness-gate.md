# Pods behind a NEG wait for the load balancer through the NEG readiness gate

GKE's NEG controller gives pods behind a NEG the readiness gate `cloud.google.com/load-balancer-neg-ready`, and holds the pod condition of that type until the load balancer finds the pod's network endpoint HEALTHY. The emulator's NEG controller had no gate, so a rolling update moved on as soon as the pod was containers ready. With surge 1 and unavailable 0, one refresh of a backend service's endpoints could drain the last HEALTHY endpoint and add the new one as UNKNOWN, and the load balancer answered `503 failed_to_pick_backend` until the new endpoint's first probe returned (#1036). The load balancer is right to answer 503 then, so the emulator now has the gate, and the load balancer is unchanged.

A mutating webhook adds `{conditionType: cloud.google.com/load-balancer-neg-ready}` to `spec.readinessGates` when a pod is created and a Service in its namespace selects it, provided that Service either has a `cloud.google.com/neg` annotation with `exposed_ports` or has its NEGs used by a GKE Ingress or Gateway load balancer. This holds on Standard and Autopilot clusters. Pods in `kube-system` are excluded. Like the CA injector and Instrumentation webhooks, the Instance serves the webhook through the Google front end with `failurePolicy: Ignore`, so it fails open. If the Instance can't be reached or the Services can't be read, the pod is admitted without the gate and becomes pod Ready as it did before. Readiness gates can't be changed after a pod is created, so pods that already exist never get the gate.

A gated pod's EndpointSlice condition `ready`, and `serving` along with it, follows pod Ready, which includes the gate. So a gated pod joins its NEG once its `ContainersReady` condition is True and it is not terminating. Otherwise its gate could never go True. Every other endpoint joins as its EndpointSlice says, as before, and a terminating pod leaves the NEG as before.

The NEG sync sets the condition True, with `reason` and `message` naming the case, following `getExpectedNegCondition` in ingress-gce's `pkg/neg/readiness/reflector.go`:

- `LoadBalancerNegReady` when the endpoint is HEALTHY in **any** backend service with a health check that has one of the pod's NEGs as a backend;
- `LoadBalancerNegWithoutHealthCheck` as soon as the endpoint is in a NEG that no backend service with a health check uses;
- `LoadBalancerNegReady` ("Pod does not belong to any NEG.") once the pod is containers ready, when no NEG the controller owns covers it, for example because the Service lost its annotation, was deleted or changed its selector, or because the gate was added by hand;
- `LoadBalancerNegTimeout` when none of these holds 10 minutes after the pod's creation. That is ingress-gce's `unreadyTimeout = 10 * time.Minute`. GKE's documentation lists the timeout as a case but gives no length, so the value is the reflector's.

The reasons and messages are the reflector's. ingress-gce reports a pod that no NEG covers as ready without waiting for its containers; the emulator waits for its containers, as #1036 asks. Once True, the condition is never set back to False. ingress-gce never patches it to False either. The emulator writes no False `LoadBalancerNegNotReady` condition while it waits; the missing condition already holds the pod back.

The load balancer's view comes from a new cross-service peer, `emu.NEGHealth`, which the lb Service implements. For one NEG endpoint, it lists each backend service with a health check that uses the NEG, with the endpoint's state there as `backendServices.getHealth` reports it. Every backend service is in the load balancer's dataplane, so one without a forwarding rule counts. Without the lb Service, every NEG counts as having no health check.

Source: [kubernetes/ingress-gce `pkg/neg/readiness/reflector.go` at 9369cac](https://github.com/kubernetes/ingress-gce/blob/9369cacf7228c9855a16137ea419c4919ac24fa2/pkg/neg/readiness/reflector.go), and GKE's [Pod readiness](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/ingress-routing-security) section, which lists the cases.

## Considered options

- **Leave the gate out and make the load balancer fail open, or keep a DRAINING endpoint in rotation.** That would hide the gap instead of closing it, and Google's load balancer does neither.
- **Fail closed when the Instance can't be reached.** Pods could not be created while the Instance is down, and the CA injector and Instrumentation webhooks already fail open.
- **Join on the EndpointSlice's `serving` condition.** Kubernetes derives `serving` from pod Ready too, so a gated pod would never join.
- **Require HEALTHY in every health-checked backend service.** GKE requires it in any one, and a NEG shared by a backend service that probes another port would hold rollouts back.
- **Let a pod that no NEG covers wait for the timeout.** A gate left behind by a deleted Service would stall each rollout for 10 minutes.
- **Add a switch to turn the gate off.** GKE has none, and a switch would let tests miss what users see.

## Consequences

- Rollouts behind a health-checked NEG take longer by the time the load balancer takes to find each new endpoint HEALTHY, about the health check's interval times its healthy threshold.
- A pod created while the Instance is down, or before its Service exists, has no gate and is never held back.
- A pod whose endpoint never becomes HEALTHY becomes pod Ready 10 minutes after it was created, with `LoadBalancerNegTimeout`.
- Each NEG sync now lists the cluster's pods once per second, read from the API server's cache.
