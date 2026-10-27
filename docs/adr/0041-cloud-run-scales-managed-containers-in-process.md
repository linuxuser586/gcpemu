# Cloud Run scales Managed containers from inside the Instance

Cloud Run joins the Products for v1.0. We decided that each Cloud Run instance of a revision is a Managed container, started and stopped by an autoscaler in the Instance, with an in-process proxy in front that enforces `containerConcurrency`, the request timeout, scale to zero, minimum and maximum instances, traffic splits and tags, and `run.invoker`. The proxy reuses the load balancer's L7 proxy and the per-Instance container plumbing.

## Considered options

- **Knative Serving on a hidden k3s cluster**, with the v1 API mapped onto Knative objects. Its semantics are closest to Cloud Run's v1 surface, but every Instance using Cloud Run would carry a Kubernetes control plane's memory and start-up time, and the emulator would be debugging Knative rather than emulating Cloud Run.

## Consequences

- One model of Cloud Run services and revisions is served over both v2 and the Knative-style v1 surface; the v1 surface is a wire format, not Knative.
- Cold starts are real container starts, so scale to zero costs a request the container's start-up time, as on GCP.
- Cloud Run is a Runtime Service.
