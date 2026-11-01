module github.com/linuxuser586/gcpemu

go 1.27.1

toolchain go1.27.3

require (
	cel.dev/cel-go v0.33.2
	cloud.google.com/go/artifactregistry v1.28.1
	cloud.google.com/go/bigtable v1.58.0
	cloud.google.com/go/certificatemanager v1.17.1
	cloud.google.com/go/cloudbuild v1.36.1
	cloud.google.com/go/cloudsqlconn v1.26.0
	cloud.google.com/go/compute/metadata v0.10.0
	cloud.google.com/go/container v1.56.1
	cloud.google.com/go/datastore v1.28.0
	cloud.google.com/go/firestore v1.29.0
	cloud.google.com/go/iam v1.15.1
	cloud.google.com/go/longrunning v1.4.1
	cloud.google.com/go/monitoring v1.32.1
	cloud.google.com/go/networksecurity v0.21.1
	cloud.google.com/go/pubsub/v2 v2.8.0
	cloud.google.com/go/secretmanager v1.23.1
	cloud.google.com/go/spanner v1.96.1
	cloud.google.com/go/storage v1.70.0
	github.com/BurntSushi/toml v1.6.0
	github.com/Microsoft/go-winio v0.6.4
	github.com/bufbuild/protocompile v0.14.1
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/go-containerregistry v0.22.1
	github.com/googleapis/gax-go/v2 v2.27.0
	github.com/hamba/avro/v2 v2.31.0
	github.com/hashicorp/yamux v0.1.2
	github.com/jackc/pgx/v5 v5.12.0
	github.com/miekg/dns v1.1.73
	github.com/prometheus/client_model v0.6.3
	github.com/prometheus/common v0.73.1
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	go.etcd.io/bbolt v1.5.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.46.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/metric v1.47.0
	go.opentelemetry.io/proto/otlp v1.11.1
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/net v0.62.0
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sys v0.49.0
	google.golang.org/api v0.310.0
	google.golang.org/genproto v0.0.0-20261026165753-2ffab32bce7e
	google.golang.org/genproto/googleapis/api v0.0.0-20261026165753-2ffab32bce7e
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261026165753-2ffab32bce7e
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	sigs.k8s.io/secrets-store-csi-driver v1.6.1
)

require (
	cel.dev/expr v0.25.3 // indirect
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.24.2 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.3.1 // indirect
	cloud.google.com/go/sql v0.5.0 // indirect
	github.com/GoogleCloudPlatform/grpc-gcp-go/grpcgcp v1.6.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric v0.62.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping v0.62.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cncf/xds/go v0.0.0-20260202195803-dba9d589def2 // indirect
	github.com/docker/cli v29.9.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.12 // indirect
	github.com/envoyproxy/go-control-plane/envoy v1.40.0 // indirect
	github.com/envoyproxy/protoc-gen-validate v1.3.3 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.7 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/s2a-go v0.1.11 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.24 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.33.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pandatix/go-abnf v0.5.5 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/spiffe/go-spiffe/v2 v2.9.0 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/detectors/gcp v1.47.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.72.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)
