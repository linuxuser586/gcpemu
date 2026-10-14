// Probes the Node Pub/Sub client with no endpoint override (#246): it
// prints "pubsub-no-override OK <topic>" or "pubsub-no-override ERR
// <error>". The compat suite sets the mTLS variables and runs it behind the
// Escape guard, which names the host the client dialed.
const {PubSub} = require('@google-cloud/pubsub');
const [projectId, suffix] = process.argv.slice(2);
process.on('unhandledRejection', () => {});

(async () => {
  try {
    const [t] = await new PubSub({projectId, universeDomain: 'gcpemu.invalid'})
      .createTopic(`node-mtls-${suffix}`, {timeout: 10000, maxRetries: 0});
    console.log('pubsub-no-override OK', t.name);
  } catch (e) {
    console.log('pubsub-no-override ERR', String(e.message).replace(/\s+/g, ' ').slice(0, 300));
  }
  process.exit(0);
})();
