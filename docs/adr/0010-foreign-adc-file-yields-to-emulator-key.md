# gcloud's application-default credentials yield to an emulator key

ADC looks for gcloud's application-default credentials (`application_default_credentials.json`, written by `gcloud auth application-default login`) before it asks the metadata server. They are Foreign credentials: their token requests go to `oauth2.googleapis.com`, and Go HTTP clients, Python Cloud Storage with credentials passed explicitly and the Node REST fallback all fetch a token before they check the universe. So on any machine where someone had logged in, `eval "$(gcpemu env)"` and then ADC sent an Escaped request, with the developer's real refresh token, even though every API had an Endpoint override (#247). We decided that `gcpemu env` exports `GOOGLE_APPLICATION_CREDENTIALS`, pointing at an emulator-issued key for the metadata server's service account, when that file exists and `GOOGLE_APPLICATION_CREDENTIALS` is unset. The Instance writes the key to its data directory on request (`POST /_emu/v1/adc-key`) and reuses it while its key is valid. A `GOOGLE_APPLICATION_CREDENTIALS` the user set is never overridden; `gcpemu env` and `gcpemu doctor` warn when it holds Foreign credentials.

## Considered options

- **Always export the key.** `gcpemu env` would print the same on every machine, but nobody would use metadata ADC, which ADR 0009 keeps working for host processes.
- **Point `CLOUDSDK_CONFIG` at an empty directory.** Python would no longer find the file, but Go and Node look only under the home directory, and gcloud would lose its configuration, defeating `--no-universe`.
- **An `authorized_user` file pointing at the emulator.** Python google-auth always sends user credentials' token requests to Google, whatever the file's `token_uri`, and Go reads no `token_uri` from it.
- **Warn only.** That leaves the documented default escaping.

## Consequences

- `gcpemu env` prints differently on machines with gcloud's application-default credentials. There, ADC uses a key instead of the metadata server, with the same service account, so the same IAM rights.
- The key is a user-managed key on that account, listed with its other keys. Reset removes it, and the next `gcpemu env` writes a new one, replacing a `GOOGLE_APPLICATION_CREDENTIALS` that names the old file.
- Python Cloud Storage with those credentials passed explicitly needs `client_options={"universe_domain": "gcpemu.invalid"}`, as with any emulator key.
