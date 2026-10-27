// Probes the Java Storage and Pub/Sub client libraries with only the
// variables `gcpemu env` sets: each line is "<case> OK <result>" or
// "<case> ERR <error>". With a third argument, "canary", it runs only the
// cases without an endpoint override, whose requests the Escape guard must
// refuse.

import com.google.api.gax.core.NoCredentialsProvider;
import com.google.api.gax.grpc.GrpcTransportChannel;
import com.google.api.gax.rpc.FixedTransportChannelProvider;
import com.google.api.gax.rpc.TransportChannelProvider;
import com.google.auth.Credentials;
import com.google.cloud.ServiceOptions;
import com.google.cloud.pubsub.v1.MessageReceiver;
import com.google.cloud.pubsub.v1.Publisher;
import com.google.cloud.pubsub.v1.Subscriber;
import com.google.cloud.pubsub.v1.SubscriptionAdminClient;
import com.google.cloud.pubsub.v1.SubscriptionAdminSettings;
import com.google.cloud.pubsub.v1.TopicAdminClient;
import com.google.cloud.pubsub.v1.TopicAdminSettings;
import com.google.cloud.storage.BlobId;
import com.google.cloud.storage.BlobInfo;
import com.google.cloud.storage.BucketInfo;
import com.google.cloud.storage.Storage;
import com.google.cloud.storage.StorageOptions;
import com.google.protobuf.ByteString;
import com.google.pubsub.v1.PubsubMessage;
import com.google.pubsub.v1.PushConfig;
import com.google.pubsub.v1.SubscriptionName;
import com.google.pubsub.v1.TopicName;
import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;

public class Probe {
  interface Case {
    Object run() throws Exception;
  }

  static void check(String name, Case c) {
    try {
      System.out.println(name + " OK " + c.run());
    } catch (Throwable e) {
      String msg = String.join(" ", String.valueOf(e.getMessage()).split("\\s+"));
      System.out.println(name + " ERR " + e.getClass().getSimpleName() + " " + msg.substring(0, Math.min(msg.length(), 300)));
    }
    System.out.flush();
  }

  static void require(boolean ok, String what) {
    if (!ok) {
      throw new IllegalStateException(what);
    }
  }

  public static void main(String[] args) throws Exception {
    String project = args[0], name = "java-compat-" + args[1];
    if (args.length > 2 && args[2].equals("canary")) {
      check("storage-no-override", () -> storageNoOverride(project, name));
      check("pubsub-no-override", () -> pubsubNoOverride(project, name));
      return;
    }
    check("storage", () -> storage(project, name));
    check("pubsub", () -> pubsub(project, name));
  }

  // Bucket create, a binary object written and read back byte for byte,
  // list and delete, on the Storage host override. The credentials are
  // ADC: in a `gcpemu env` shell, the metadata server or an emulator key,
  // both in the Invalid universe that GOOGLE_CLOUD_UNIVERSE_DOMAIN names.
  static Object storage(String project, String bucket) throws Exception {
    Storage s = StorageOptions.newBuilder()
        .setHost("http://" + System.getenv("STORAGE_EMULATOR_HOST"))
        .setProjectId(project)
        .build()
        .getService();
    s.create(BucketInfo.of(bucket));
    byte[] content = {0, 1, 2, (byte) 0xfe, (byte) 0xff, '\n', '\r', 0, 'j', 'v', 'm'};
    BlobId id = BlobId.of(bucket, "dir/bytes.bin");
    s.create(BlobInfo.newBuilder(id).setContentType("application/octet-stream").build(), content);
    byte[] got = s.readAllBytes(id);
    require(Arrays.equals(got, content), "read " + Arrays.toString(got) + ", want " + Arrays.toString(content));
    List<String> names = new ArrayList<>();
    s.list(bucket).iterateAll().forEach(b -> names.add(b.getName()));
    require(names.equals(List.of("dir/bytes.bin")), "listed " + names);
    require(s.delete(id), "delete found no object");
    require(!s.list(bucket).iterateAll().iterator().hasNext(), "object listed after delete");
    require(s.delete(bucket), "delete found no bucket");
    return bucket;
  }

  // Topic and subscription create, one message published with an
  // attribute and received through the streaming-pull Subscriber, which
  // acks it and shuts down cleanly. The plaintext channel and
  // NoCredentialsProvider are what the Pub/Sub emulator docs describe:
  // grpc-java sends no credentials over plaintext. gax compares missing
  // credentials with Google's universe, so each client names it, or the
  // GOOGLE_CLOUD_UNIVERSE_DOMAIN from `gcpemu env` fails every call.
  static Object pubsub(String project, String name) throws Exception {
    ManagedChannel channel = ManagedChannelBuilder.forTarget(System.getenv("PUBSUB_EMULATOR_HOST")).usePlaintext().build();
    try {
      TransportChannelProvider channels = FixedTransportChannelProvider.create(GrpcTransportChannel.create(channel));
      NoCredentialsProvider creds = NoCredentialsProvider.create();
      String universe = Credentials.GOOGLE_DEFAULT_UNIVERSE;
      TopicName topic = TopicName.of(project, name);
      SubscriptionName sub = SubscriptionName.of(project, name);
      try (TopicAdminClient topics = TopicAdminClient.create(TopicAdminSettings.newBuilder()
              .setTransportChannelProvider(channels).setCredentialsProvider(creds).setUniverseDomain(universe).build());
          SubscriptionAdminClient subs = SubscriptionAdminClient.create(SubscriptionAdminSettings.newBuilder()
              .setTransportChannelProvider(channels).setCredentialsProvider(creds).setUniverseDomain(universe).build())) {
        topics.createTopic(topic);
        subs.createSubscription(sub, topic, PushConfig.getDefaultInstance(), 10);
      }

      Publisher pub = Publisher.newBuilder(topic)
          .setChannelProvider(channels).setCredentialsProvider(creds).setUniverseDomain(universe).build();
      String id;
      try {
        id = pub.publish(PubsubMessage.newBuilder()
            .setData(ByteString.copyFromUtf8("hi from java"))
            .putAttributes("k", "v")
            .build()).get(30, TimeUnit.SECONDS);
      } finally {
        pub.shutdown();
        pub.awaitTermination(30, TimeUnit.SECONDS);
      }

      CompletableFuture<PubsubMessage> received = new CompletableFuture<>();
      MessageReceiver receiver = (m, consumer) -> {
        consumer.ack();
        received.complete(m);
      };
      Subscriber subscriber = Subscriber.newBuilder(sub.toString(), receiver)
          .setChannelProvider(channels).setCredentialsProvider(creds).setUniverseDomain(universe).build();
      subscriber.startAsync().awaitRunning(30, TimeUnit.SECONDS);
      PubsubMessage msg;
      try {
        msg = received.get(60, TimeUnit.SECONDS);
      } finally {
        subscriber.stopAsync().awaitTerminated(30, TimeUnit.SECONDS);
      }
      require(msg.getMessageId().equals(id), "received message " + msg.getMessageId() + ", published " + id);
      require(msg.getData().toStringUtf8().equals("hi from java"), "data " + msg.getData().toStringUtf8());
      require("v".equals(msg.getAttributesMap().get("k")), "attributes " + msg.getAttributesMap());
      return id;
    } finally {
      channel.shutdownNow().awaitTermination(10, TimeUnit.SECONDS);
    }
  }

  // The canaries. Without setHost, Storage sends its request to Google:
  // the library ignores GOOGLE_CLOUD_UNIVERSE_DOMAIN.
  static Object storageNoOverride(String project, String name) {
    Storage s = StorageOptions.newBuilder()
        .setProjectId(project)
        .setRetrySettings(ServiceOptions.getNoRetrySettings())
        .build()
        .getService();
    return s.get(name);
  }

  // Without the emulator channel, Pub/Sub dials its default endpoint.
  static Object pubsubNoOverride(String project, String name) throws Exception {
    TopicAdminSettings.Builder settings = TopicAdminSettings.newBuilder();
    settings.getTopicSettings().setSimpleTimeoutNoRetriesDuration(Duration.ofSeconds(10));
    try (TopicAdminClient topics = TopicAdminClient.create(settings.build())) {
      return topics.getTopic(TopicName.of(project, name)).getName();
    }
  }
}
