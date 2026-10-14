import { describe, expect, it } from 'vitest'

import {
  block,
  curl,
  expr,
  gcloud,
  hclString,
  jsonencode,
  resource,
  shQuote,
  tofuName,
} from './copyAs'

describe('shQuote', () => {
  it('leaves plain words alone and quotes the rest', () => {
    expect(shQuote('projects/p/topics/t')).toBe('projects/p/topics/t')
    expect(shQuote('env=dev,team=a')).toBe('env=dev,team=a')
    expect(shQuote('a b')).toBe("'a b'")
    expect(shQuote("it's")).toBe("'it'\\''s'")
    expect(shQuote('')).toBe("''")
  })
})

describe('gcloud', () => {
  it('writes flags in order, one per line, skipping unset ones', () => {
    expect(
      gcloud(['pubsub', 'topics', 'create', 'orders'], {
        project: 'p-1',
        labels: ['a=1', 'b=2'],
        none: undefined,
        off: false,
        empty: '',
        list: [],
        'retain-acked-messages': true,
        'message-filter': 'attributes.kind = "x"',
        'ack-deadline': 30,
      }),
    ).toBe(
      [
        'gcloud pubsub topics create orders \\',
        '    --project=p-1 \\',
        '    --labels=a=1,b=2 \\',
        '    --retain-acked-messages \\',
        `    --message-filter='attributes.kind = "x"' \\`,
        '    --ack-deadline=30',
      ].join('\n'),
    )
  })

  it('is one line with no flags', () => {
    expect(gcloud(['sql', 'databases', 'create', 'my db'])).toBe(
      "gcloud sql databases create 'my db'",
    )
  })
})

describe('curl', () => {
  it('sends a JSON body to the gateway', () => {
    expect(
      curl('http://127.0.0.1:4443', {
        method: 'PUT',
        path: '/pubsub/v1/projects/p/topics/t',
        body: { labels: { a: "it's" } },
      }),
    ).toBe(
      [
        'curl http://127.0.0.1:4443/pubsub/v1/projects/p/topics/t \\',
        '    -X PUT \\',
        "    -H 'Content-Type: application/json' \\",
        "    -d '{",
        '  "labels": {',
        `    "a": "it'\\''s"`,
        '  }',
        "}'",
      ].join('\n'),
    )
  })

  it('uploads a local file', () => {
    expect(
      curl('http://h', {
        method: 'POST',
        path: '/upload/storage/v1/b/b/o?uploadType=media&name=a.txt',
        file: { path: './my file.txt', contentType: 'text/plain' },
      }),
    ).toBe(
      [
        "curl 'http://h/upload/storage/v1/b/b/o?uploadType=media&name=a.txt' \\",
        '    -X POST \\',
        "    -H 'Content-Type: text/plain' \\",
        "    --data-binary '@./my file.txt'",
      ].join('\n'),
    )
  })

  it('quotes a URL with a query string', () => {
    expect(curl('http://h', { method: 'POST', path: '/storage/v1/b?project=p' })).toBe(
      "curl 'http://h/storage/v1/b?project=p' \\\n    -X POST",
    )
  })
})

describe('HCL', () => {
  it('escapes strings and template sequences', () => {
    expect(hclString('a "b" \\ ${c} %{d}\n')).toBe('"a \\"b\\" \\\\ $${c} %%{d}\\n"')
  })

  it('makes resource IDs into resource names', () => {
    expect(tofuName('orders')).toBe('orders')
    expect(tofuName('my.zone-1')).toBe('my_zone-1')
    expect(tofuName('1st')).toBe('_1st')
  })

  it('writes a resource as tofu fmt does', () => {
    expect(
      resource('google_pubsub_subscription', 'orders.worker', {
        project: 'p',
        name: 'orders.worker',
        unset: undefined,
        no_labels: {},
        ack_deadline_seconds: 30,
        retain_acked_messages: true,
        labels: { env: 'dev', 'x-y': 'z', 'a.b/c': 'd' },
        schema: jsonencode({ type: 'record', fields: [{ name: 'id' }] }),
        topic: expr('google_pubsub_topic.t.id'),
        expiration_policy: block({ ttl: '' }),
        empty: block({}),
        rule: [block({ a: 1 }), block({ a: 2 })],
        ports: ['80', '443'],
      }),
    ).toBe(
      [
        'resource "google_pubsub_subscription" "orders_worker" {',
        '  project               = "p"',
        '  name                  = "orders.worker"',
        '  ack_deadline_seconds  = 30',
        '  retain_acked_messages = true',
        '  labels                = { env = "dev", x-y = "z", "a.b/c" = "d" }',
        '  schema                = jsonencode({ type = "record", fields = [{ name = "id" }] })',
        '  topic                 = google_pubsub_topic.t.id',
        '  ports                 = ["80", "443"]',
        '  expiration_policy {',
        '    ttl = ""',
        '  }',
        '  empty {}',
        '  rule {',
        '    a = 1',
        '  }',
        '  rule {',
        '    a = 2',
        '  }',
        '}',
      ].join('\n'),
    )
  })

  it('breaks a long map over lines and restarts alignment after it', () => {
    expect(
      resource('t', 'n', {
        a: 1,
        labels: { environment: 'production', team: 'payments', owner: 'someone' },
        long_name: 2,
      }),
    ).toBe(
      [
        'resource "t" "n" {',
        '  a      = 1',
        '  labels = {',
        '    environment = "production"',
        '    team        = "payments"',
        '    owner       = "someone"',
        '  }',
        '  long_name = 2',
        '}',
      ].join('\n'),
    )
  })
})
