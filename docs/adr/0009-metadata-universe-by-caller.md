# The metadata server's universe depends on the caller

The metadata server reports a universe domain (`universe/universe-domain`) that Compute Engine credentials adopt. Python google-auth 2.55 and later, and Go's auth library when `GOOGLE_AUTH_TRUST_BOUNDARY_ENABLED` is set, use it to decide whether to look up the account's Regional Access Boundary at the hard-coded `iamcredentials.googleapis.com`. No Endpoint override covers that host, and nothing turns the lookup off. We decided that the metadata server reports the Invalid universe to host processes and Google's universe to GKE pods. A host process reaches the metadata server only through the `GCE_METADATA_HOST` that `gcpemu env` prints, alongside `GOOGLE_CLOUD_UNIVERSE_DOMAIN=gcpemu.invalid`, so it skips the lookup instead of sending an Escaped request. A pod addresses real hostnames through the Google front end, so its lookup reaches the emulator, which answers that no boundary applies.

## Considered options

- **Google's universe for every caller**, as before #244. The default path in the README, `eval "$(gcpemu env)"` and then ADC on the host, sent the account's email and an emulator token to Google.
- **The Invalid universe for every caller.** Pod clients would then address `<api>.gcpemu.invalid` instead of the real hostnames the Google front end serves.
- **Documentation only**, telling Python users to set `GOOGLE_APPLICATION_CREDENTIALS` to an emulator key. That leaves the documented default escaping.

## Consequences

- Host mode does not change the host answer. It serves no metadata, and `gcpemu env` gives host processes the Invalid universe in Host mode too.
- Python Cloud Storage with Compute Engine credentials passed explicitly needs `client_options={"universe_domain": "gcpemu.invalid"}`, as with an emulator key. ADC through `STORAGE_EMULATOR_HOST` does not.
- The OpenTofu provider does not read the metadata server's universe, so its blocks are unaffected.
