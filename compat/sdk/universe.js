// Probes the Node Pub/Sub client with an emulator key in ADC: each line is
// "<case> OK <result>" or "<case> ERR <error>".
const {PubSub} = require('@google-cloud/pubsub');
const [projectId, suffix] = process.argv.slice(2);
// A client whose stub creation fails also rejects its other stubs in the
// background; the case has already recorded that error.
process.on('unhandledRejection', () => {});

async function probe(name, opts, topic) {
  try {
    const [t] = await new PubSub({projectId, ...opts}).createTopic(topic, {timeout: 10000, maxRetries: 0});
    console.log(name, 'OK', t.name);
  } catch (e) {
    console.log(name, 'ERR', String(e.message).replace(/\s+/g, ' ').slice(0, 300));
  }
}

(async () => {
  await probe('pubsub-emulator', {}, `node-a-${suffix}`);
  // Any universeDomain turns emulator mode off unless emulatorMode is set.
  await probe('pubsub-emulator-universe', {universeDomain: 'gcpemu.invalid', emulatorMode: true}, `node-b-${suffix}`);
  delete process.env.PUBSUB_EMULATOR_HOST;
  delete process.env.CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB;
  await probe('pubsub-no-override', {universeDomain: 'gcpemu.invalid'}, `node-c-${suffix}`);
  process.exit(0);
})();
