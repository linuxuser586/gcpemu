# Probes the Python client libraries with an emulator key in ADC: each
# line is "<case> OK <result>" or "<case> ERR <error>".
import os, sys
from google.oauth2 import service_account
from google.cloud import pubsub_v1, storage

key, project, suffix = sys.argv[1:4]


def case(name, fn):
    try:
        print(name, "OK", fn(), flush=True)
    except Exception as e:
        print(name, "ERR", type(e).__name__, " ".join(str(e).split())[:300], flush=True)


creds = service_account.Credentials.from_service_account_file(key, scopes=["https://www.googleapis.com/auth/cloud-platform"])
case("storage-adc", lambda: storage.Client(project=project).create_bucket("py-universe-" + suffix).name)
case("storage-explicit", lambda: [b.name for b in storage.Client(project=project, credentials=creds).list_buckets()])
case("storage-explicit-universe", lambda: [b.name for b in storage.Client(
    project=project, credentials=creds, client_options={"universe_domain": "gcpemu.invalid"}).list_buckets()])
case("pubsub-emulator", lambda: pubsub_v1.PublisherClient().create_topic(name=f"projects/{project}/topics/py-universe-{suffix}").name)
del os.environ["PUBSUB_EMULATOR_HOST"]
case("pubsub-no-override", lambda: pubsub_v1.PublisherClient().get_topic(
    topic=f"projects/{project}/topics/py-universe-{suffix}", timeout=10, retry=None).name)
