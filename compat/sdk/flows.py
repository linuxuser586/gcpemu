# Runs a Cloud Storage and a Pub/Sub flow through the Python client
# libraries, configured only by the environment `gcpemu env` sets: each
# step prints "<case> OK <result>" or "<case> ERR <error>", and a flow
# stops at its first error. With a third argument, "canary", it instead
# sends one Storage and one Pub/Sub request that no endpoint override
# covers, which the Escape guard must refuse.
import os, sys, threading
from google.cloud import pubsub_v1, storage

project, suffix = sys.argv[1:3]
canary = sys.argv[3:] == ["canary"]
# Every byte value, and more than one chunk of the transport's reads.
object_bytes = bytes(range(256)) * 300
message_data, attrs = b"flow \x00\xff\r\n", {"origin": "python"}
# How long the subscriber waits for the message.
receive_timeout = 30


def flow(*steps):
    for name, fn in steps:
        try:
            print(name, "OK", fn(), flush=True)
        except Exception as e:
            print(name, "ERR", type(e).__name__, " ".join(str(e).split())[:300], flush=True)
            return


def expect(cond, what):
    if not cond:
        raise AssertionError(what)


if canary:
    del os.environ["STORAGE_EMULATOR_HOST"], os.environ["PUBSUB_EMULATOR_HOST"]
    flow(("storage-no-override", lambda: storage.Client.create_anonymous_client().bucket(f"py-flows-{suffix}").exists(
        timeout=10, retry=None)))
    flow(("pubsub-no-override", lambda: pubsub_v1.PublisherClient().get_topic(
        topic=f"projects/{project}/topics/py-flows-{suffix}", timeout=10, retry=None).name))
    sys.exit()

gcs = storage.Client(project=project)
bucket = gcs.bucket(f"py-flows-{suffix}")
blob = bucket.blob("flows/object.bin")


def read():
    got = blob.download_as_bytes()
    expect(got == object_bytes, f"read {len(got)} bytes that differ from the {len(object_bytes)} written")
    return len(got)


def names():
    return [b.name for b in gcs.list_blobs(bucket)]


def listed():
    got = names()
    expect(blob.name in got, f"{blob.name} not in {got}")
    return got


def gone():
    expect(not blob.exists(), f"{blob.name} still exists")
    got = names()
    expect(blob.name not in got, f"{blob.name} still in {got}")
    return got


flow(
    ("storage-create-bucket", lambda: gcs.create_bucket(bucket).name),
    ("storage-write", lambda: blob.upload_from_string(object_bytes, content_type="application/octet-stream") or len(object_bytes)),
    ("storage-read", read),
    ("storage-list", listed),
    ("storage-delete", lambda: blob.delete() or blob.name),
    ("storage-gone", gone),
)

publisher, subscriber = pubsub_v1.PublisherClient(), pubsub_v1.SubscriberClient()
topic = publisher.topic_path(project, f"py-flows-{suffix}")
sub = subscriber.subscription_path(project, f"py-flows-{suffix}")


def receive():
    """Receives the message through the streaming-pull subscriber, acks it
    and closes the stream."""
    got, done = [], threading.Event()

    def callback(m):
        got.append(m)
        m.ack()
        done.set()

    future = subscriber.subscribe(sub, callback)
    arrived = done.wait(receive_timeout)
    future.cancel()
    future.result(timeout=30)  # returns once the stream has shut down
    expect(arrived, f"no message within {receive_timeout}s")
    m = got[0]
    expect(m.data == message_data, f"data {m.data!r}, want {message_data!r}")
    expect(dict(m.attributes) == attrs, f"attributes {dict(m.attributes)}, want {attrs}")
    return m.message_id


def close():
    subscriber.close()
    publisher.stop()
    return "closed"


flow(
    ("pubsub-create-topic", lambda: publisher.create_topic(name=topic).name),
    ("pubsub-create-subscription", lambda: subscriber.create_subscription(name=sub, topic=topic).name),
    ("pubsub-publish", lambda: publisher.publish(topic, message_data, **attrs).result(timeout=30)),
    ("pubsub-receive", receive),
    ("pubsub-close", close),
)
