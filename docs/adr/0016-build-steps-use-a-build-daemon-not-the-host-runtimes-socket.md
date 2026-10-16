# Build steps use a Build daemon, not the host runtime's socket

Cloud Build's `gcr.io/cloud-builders/docker` steps expect a Docker daemon at `/var/run/docker.sock`, and most build configs build and push images through it. Mounting the host runtime's socket into Build steps would be the obvious way to give them one, but `docker push LOCATION-docker.pkg.dev/...` from the user's daemon resolves real Google and becomes an Escaped request unless the Instance runs in Host mode. It would also leave containers and images in the user's daemon that no Instance owns, and it would not work on Podman. We decided that every Instance runs one privileged Docker-in-Docker Managed container, the Build daemon. Its socket is mounted into every Build step, and it resolves `*-docker.pkg.dev` to the Build agent's registry proxy, which pushes and pulls as the Build's service account over plain HTTP, the way GKE nodes mirror Artifact Registry.

## Considered options

- **The host runtime's socket**, rejected for the Escaped requests and ownership problems above.
- **A Build daemon per Build**, which isolates Builds as GCP's default pool does but starts every Build with an empty layer cache, so a repeated `gcloud builds submit` is as slow as the first.
- **No daemon**, with Docker-builder steps Absent and users switched to kaniko or buildah. That breaks the unmodified build configs that are the point of the Product.

## Consequences

- Builds share one image cache, so a Build can see images an earlier Build pulled or built. GCP never allows this, and it is listed as a fidelity gap in SRS §10.2.
- The cache lives in a volume that survives Reset and restarts of a Persistent Instance (as FR-GKE-006 keeps the GKE node image cache), and it is removed with the Instance.
- The Build daemon needs a privileged container, so Cloud Build is a Runtime Service that a runtime refusing privileged containers cannot run.
