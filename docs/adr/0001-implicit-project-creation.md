# Projects are created implicitly by default

Any Project ID a request references is created in the Instance on first use, and Strict projects is an opt-in. We chose this because the main users (CI pipelines, OpenTofu configs, SDK tests) should run against a fresh Instance with zero setup, and real GCP project IDs vary per developer and environment. The cost is that a typo in a Project ID silently creates a second, empty Project instead of failing. Users who want that typo to fail can turn on Strict projects, which requires Projects to be declared in a Seed file.
