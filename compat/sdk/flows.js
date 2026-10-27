// Runs a Cloud Storage and a Pub/Sub flow through the Node client
// libraries, configured only by the environment `gcpemu env` sets: each
// step prints "<case> OK <result>" or "<case> ERR <error>", and a flow
// stops at its first error. With a third argument, "canary", it instead
// sends one Storage and one Pub/Sub request that no endpoint override
// covers, which the Escape guard must refuse.
const {PubSub} = require('@google-cloud/pubsub');
const {Storage} = require('@google-cloud/storage');
const [projectId, suffix, mode] = process.argv.slice(2);
// Every byte value, and more than one chunk of the transport's reads.
const objectBytes = Buffer.alloc(256 * 300, 0).map((_, i) => i % 256);
const messageData = Buffer.from('flow \x00\xff\r\n', 'latin1');
const attrs = {origin: 'node'};
// How long the subscriber waits for the message.
const receiveTimeout = 30000;
// A client whose stub creation fails also rejects its other stubs in the
// background; the step has already recorded that error.
process.on('unhandledRejection', () => {});

async function flow(...steps) {
  for (const [name, fn] of steps) {
    try {
      console.log(name, 'OK', JSON.stringify(await fn()));
    } catch (e) {
      console.log(name, 'ERR', String(e.message).replace(/\s+/g, ' ').slice(0, 300));
      return;
    }
  }
}

function expect(cond, what) {
  if (!cond) throw new Error(what);
}

async function canary() {
  delete process.env.STORAGE_EMULATOR_HOST;
  delete process.env.PUBSUB_EMULATOR_HOST;
  // The Pub/Sub client also takes gcloud's override.
  delete process.env.CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB;
  await flow(['storage-no-override', async () =>
    (await new Storage({projectId, retryOptions: {autoRetry: false}}).bucket(`node-flows-${suffix}`).exists())[0]]);
  await flow(['pubsub-no-override', async () =>
    (await new PubSub({projectId}).createTopic(`node-flows-${suffix}`, {timeout: 10000, maxRetries: 0}))[0].name]);
}

async function storageFlow() {
  // The library sends JSON API calls to STORAGE_EMULATOR_HOST as it is,
  // without a scheme or /storage/v1, so the endpoint goes in apiEndpoint,
  // which the library documents instead.
  const apiEndpoint = `http://${process.env.STORAGE_EMULATOR_HOST}`;
  delete process.env.STORAGE_EMULATOR_HOST;
  const gcs = new Storage({projectId, apiEndpoint});
  const bucket = gcs.bucket(`node-flows-${suffix}`);
  const file = bucket.file('flows/object.bin');
  const names = async () => (await bucket.getFiles())[0].map(f => f.name);
  await flow(
    ['storage-create-bucket', async () => (await gcs.createBucket(bucket.name))[0].name],
    ['storage-write', async () => {
      await file.save(objectBytes, {contentType: 'application/octet-stream'});
      return objectBytes.length;
    }],
    ['storage-read', async () => {
      const [got] = await file.download();
      expect(got.equals(objectBytes), `read ${got.length} bytes that differ from the ${objectBytes.length} written`);
      return got.length;
    }],
    ['storage-list', async () => {
      const got = await names();
      expect(got.includes(file.name), `${file.name} not in ${got}`);
      return got;
    }],
    ['storage-delete', async () => {
      await file.delete();
      return file.name;
    }],
    ['storage-gone', async () => {
      expect(!(await file.exists())[0], `${file.name} still exists`);
      const got = await names();
      expect(!got.includes(file.name), `${file.name} still in ${got}`);
      return got;
    }],
  );
}

async function pubsubFlow() {
  const pubsub = new PubSub({projectId});
  let topic, sub;
  await flow(
    ['pubsub-create-topic', async () => ([topic] = await pubsub.createTopic(`node-flows-${suffix}`), topic.name)],
    ['pubsub-create-subscription', async () => ([sub] = await topic.createSubscription(`node-flows-${suffix}`), sub.name)],
    ['pubsub-publish', () => topic.publishMessage({data: messageData, attributes: attrs})],
    // The streaming-pull subscriber: listening for messages opens the
    // stream, and close ends it.
    ['pubsub-receive', async () => {
      let timer, m;
      try {
        m = await new Promise((resolve, reject) => {
          timer = setTimeout(() => reject(new Error(`no message within ${receiveTimeout / 1000}s`)), receiveTimeout);
          sub.on('message', m => {
            m.ack();
            resolve(m);
          });
          sub.on('error', reject);
        });
      } finally {
        clearTimeout(timer);
        await sub.close();
      }
      expect(m.data.equals(messageData), `data ${m.data.toString('hex')}, want ${messageData.toString('hex')}`);
      expect(JSON.stringify(m.attributes) === JSON.stringify(attrs), `attributes ${JSON.stringify(m.attributes)}, want ${JSON.stringify(attrs)}`);
      return m.id;
    }],
    ['pubsub-close', async () => {
      await pubsub.close();
      return 'closed';
    }],
  );
}

(async () => {
  if (mode === 'canary') {
    await canary();
  } else {
    await storageFlow();
    await pubsubFlow();
  }
  // Exit if a client that a failed step left open keeps the process alive;
  // exiting right away could cut off output to a pipe.
  setTimeout(() => process.exit(0), 5000).unref();
})();
