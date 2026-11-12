# An update-mask path naming a map key changes only that key

Every Service that takes an update mask parsed its paths on its own, and each got map fields wrong in a different way. Artifact Registry cut `labels.env` and `cleanup_policies.a` down to the field and replaced the whole map (#1062). Secret Manager does the same and also snake-cases the key. Certificate Manager camel-cases the key, so `labels.my_key` drops the user's change and deletes an existing `myKey`. IAM replaces a Workload Identity provider's whole `attributeMapping`. Pub/Sub and Cloud Build reject the path. All of these but the last two return OK and lose data the caller never named.

So every Service follows AIP-161 for update-mask paths into map fields:

- A path `<map field>.<key>` changes that key only. If the request's map holds the key, its value is set. If not, the key is removed. Other keys are kept.
- The key is bare (`labels.env`) or backtick-quoted (`` cleanup_policies.`delete-old` ``), and a quoted key may contain dots. It is used exactly as written. Only the field segment is converted from lowerCamel to snake_case, or the other way round.
- A bare field path (`labels`) replaces the whole map, as before. When a mask names both a field and keys of that field, the whole-field path wins.
- A key path on a field that is not a map, or one with more segments after the key, is INVALID_ARGUMENT.
- A Service's checks and limits on a map, such as Artifact Registry's 10 cleanup policies, apply to the merged map, in the same store transaction that writes it.

The rule covers update-mask paths only. Cloud DNS and Cloud SQL PATCH take no mask on a map field and keep merging or replacing by top-level body fields. Paths into nested message fields (`docker_config.immutable_tags`) are outside it, and each Service keeps its own choice to accept or reject them.

The rule follows AIP-161, not recorded behaviour of real GCP. No Conformance Recording covers map-key paths yet.

## Considered options

- **Fix each Service as a bug report arrives, with no rule.** That is how six Services came to parse masks six ways. A new Service would get no guidance.
- **Reject map-key paths everywhere, as Pub/Sub and Cloud Build did.** It loses no data, but a client that follows AIP-161, such as one updating one label, fails on the emulator.
- **Record real GCP first.** The Conformance suite could show what each API does, but it costs a real project per API, and AIP-161 already states the contract the client libraries are written against.
- **A shared helper that also merges.** Artifact Registry and Secret Manager change protos through protoreflect, while Certificate Manager and IAM change JSON-shaped maps, so one merge would have to cover both. Only the path parsing is shared, in `internal/`. Each Service keeps its own merge.

## Consequences

- Validation that used to run on the request's map now also has to run on the merged map, inside the transaction. A Service can still check the request's own map first, so a malformed request is INVALID_ARGUMENT before a missing resource is NOT_FOUND.
- Pub/Sub and Cloud Build start accepting paths they used to reject.
