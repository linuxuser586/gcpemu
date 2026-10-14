import { block, gcloud, keyValues, omit, resource, type Recipe } from '@/lib/copyAs'

import { lastSegment, projectOf, type Subscription, type Topic } from './api'

// Copy as (FR-UI-015) for topics and subscriptions: gcloud pubsub, the
// pubsub/v1 create request and the google_pubsub_* resource.

const encoding = (e?: string) => (e && e !== 'ENCODING_UNSPECIFIED' ? e.toLowerCase() : undefined)

export function topicRecipe(t: Topic): Recipe {
  const project = projectOf(t.name)
  const id = lastSegment(t.name)
  return {
    gcloud: gcloud(['pubsub', 'topics', 'create', id], {
      project,
      labels: keyValues(t.labels),
      'message-retention-duration': t.messageRetentionDuration,
      'topic-encryption-key': t.kmsKeyName,
      schema: t.schemaSettings?.schema,
      'message-encoding': encoding(t.schemaSettings?.encoding),
    }),
    rest: {
      method: 'PUT',
      path: `/pubsub/v1/${t.name}`,
      body: omit(t, ['name', 'state']),
    },
    tofu: resource('google_pubsub_topic', id, {
      project,
      name: id,
      labels: t.labels,
      message_retention_duration: t.messageRetentionDuration,
      kms_key_name: t.kmsKeyName,
      schema_settings: t.schemaSettings?.schema
        ? block({ schema: t.schemaSettings.schema, encoding: t.schemaSettings.encoding })
        : undefined,
    }),
  }
}

export function subscriptionRecipe(s: Subscription): Recipe {
  const project = projectOf(s.name)
  const id = lastSegment(s.name)
  const push = s.pushConfig?.pushEndpoint ? s.pushConfig : undefined
  const exp = s.expirationPolicy
  // An expiration policy with no TTL never expires the subscription.
  const expiration = exp ? exp.ttl || 'never' : undefined
  return {
    gcloud: gcloud(['pubsub', 'subscriptions', 'create', id], {
      project,
      topic: s.topic,
      'ack-deadline': s.ackDeadlineSeconds,
      'retain-acked-messages': s.retainAckedMessages,
      'message-retention-duration': s.messageRetentionDuration,
      labels: keyValues(s.labels),
      'enable-message-ordering': s.enableMessageOrdering,
      'expiration-period': expiration,
      'message-filter': s.filter,
      'dead-letter-topic': s.deadLetterPolicy?.deadLetterTopic,
      'max-delivery-attempts': s.deadLetterPolicy?.deadLetterTopic
        ? s.deadLetterPolicy.maxDeliveryAttempts
        : undefined,
      'min-retry-delay': s.retryPolicy?.minimumBackoff,
      'max-retry-delay': s.retryPolicy?.maximumBackoff,
      'enable-exactly-once-delivery': s.enableExactlyOnceDelivery,
      'push-endpoint': push?.pushEndpoint,
      'push-auth-service-account': push?.oidcToken?.serviceAccountEmail,
      'push-auth-token-audience': push?.oidcToken?.audience,
      'push-no-wrapper': !!push?.noWrapper,
      'push-no-wrapper-write-metadata': push?.noWrapper?.writeMetadata,
    }),
    rest: {
      method: 'PUT',
      path: `/pubsub/v1/${s.name}`,
      body: omit(s, ['name', 'state', 'detached', 'topicMessageRetentionDuration']),
    },
    tofu: resource('google_pubsub_subscription', id, {
      project,
      name: id,
      topic: s.topic,
      ack_deadline_seconds: s.ackDeadlineSeconds,
      message_retention_duration: s.messageRetentionDuration,
      retain_acked_messages: s.retainAckedMessages || undefined,
      enable_message_ordering: s.enableMessageOrdering || undefined,
      enable_exactly_once_delivery: s.enableExactlyOnceDelivery || undefined,
      filter: s.filter,
      labels: s.labels,
      expiration_policy: exp ? block({ ttl: exp.ttl ?? '' }) : undefined,
      retry_policy: s.retryPolicy
        ? block({
            minimum_backoff: s.retryPolicy.minimumBackoff,
            maximum_backoff: s.retryPolicy.maximumBackoff,
          })
        : undefined,
      dead_letter_policy: s.deadLetterPolicy?.deadLetterTopic
        ? block({
            dead_letter_topic: s.deadLetterPolicy.deadLetterTopic,
            max_delivery_attempts: s.deadLetterPolicy.maxDeliveryAttempts,
          })
        : undefined,
      push_config: push
        ? block({
            push_endpoint: push.pushEndpoint,
            attributes: push.attributes,
            oidc_token: push.oidcToken?.serviceAccountEmail
              ? block({
                  service_account_email: push.oidcToken.serviceAccountEmail,
                  audience: push.oidcToken.audience,
                })
              : undefined,
            no_wrapper: push.noWrapper
              ? block({ write_metadata: push.noWrapper.writeMetadata ?? false })
              : undefined,
          })
        : undefined,
    }),
  }
}
