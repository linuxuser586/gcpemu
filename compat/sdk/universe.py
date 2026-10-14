# Probes the Python client libraries with an emulator key in ADC, and
# google-auth with the metadata server: each line is "<case> OK <result>"
# or "<case> ERR <error>". With a fourth argument, "canary", it runs only
# the metadata case in Google's universe, whose Regional Access Boundary
# lookup the Escape guard must refuse.
import os, sys
from google.auth import compute_engine
from google.auth.transport.requests import AuthorizedSession
from google.oauth2 import service_account
from google.cloud import pubsub_v1, storage

key, project, suffix = sys.argv[1:4]
canary = sys.argv[4:] == ["canary"]


def case(name, fn):
    try:
        print(name, "OK", fn(), flush=True)
    except Exception as e:
        print(name, "ERR", type(e).__name__, " ".join(str(e).split())[:300], flush=True)


def metadata(**kw):
    """Lists buckets as the metadata server's account, then reports its
    universe. The lookup of the account's Regional Access Boundary, which
    google-auth sends to iamcredentials.googleapis.com in Google's universe,
    runs in the request instead of in the background, and fails silently."""
    creds = compute_engine.Credentials(**kw)._set_blocking_regional_access_boundary_lookup()
    url = f"http://{os.environ['GCPEMU_GATEWAY']}/storage/v1/b?project={project}"
    AuthorizedSession(creds).get(url, timeout=30).raise_for_status()
    return creds.universe_domain


if canary:
    case("metadata-google-universe", lambda: metadata(universe_domain="googleapis.com"))
    sys.exit()

creds = service_account.Credentials.from_service_account_file(key, scopes=["https://www.googleapis.com/auth/cloud-platform"])
case("storage-adc", lambda: storage.Client(project=project).create_bucket("py-universe-" + suffix).name)
case("storage-explicit", lambda: [b.name for b in storage.Client(project=project, credentials=creds).list_buckets()])
case("storage-explicit-universe", lambda: [b.name for b in storage.Client(
    project=project, credentials=creds, client_options={"universe_domain": "gcpemu.invalid"}).list_buckets()])
case("metadata", metadata)
case("pubsub-emulator", lambda: pubsub_v1.PublisherClient().create_topic(name=f"projects/{project}/topics/py-universe-{suffix}").name)
del os.environ["PUBSUB_EMULATOR_HOST"]
case("pubsub-no-override", lambda: pubsub_v1.PublisherClient().get_topic(
    topic=f"projects/{project}/topics/py-universe-{suffix}", timeout=10, retry=None).name)
